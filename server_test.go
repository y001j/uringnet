//go:build linux
// +build linux

package uringnet

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"
)

// apiEcho 原样回显；split 为 true 时把数据分两段写回，在增量缓冲区下验证同一缓冲区的多段原地发送
type apiEcho struct {
	BuiltinHandler
	split bool

	mu     sync.Mutex
	errs   []error
	opened chan *Conn
}

func (h *apiEcho) OnOpen(c *Conn) Action {
	if h.opened != nil {
		h.opened <- c
	}
	return None
}

func (h *apiEcho) OnTraffic(c *Conn, data []byte) Action {
	if h.split && len(data) > 1 {
		c.Write(data[:len(data)/2])
		c.Write(data[len(data)/2:])
		return None
	}
	c.Write(data)
	return None
}

func (h *apiEcho) OnClose(c *Conn, err error) {
	h.mu.Lock()
	h.errs = append(h.errs, err)
	h.mu.Unlock()
}

func (h *apiEcho) closeErrs() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]error(nil), h.errs...)
}

// serve 启动 Server，返回其地址；测试结束时关闭并检查 Serve 的返回值
func serve(t *testing.T, h Handler, opts ...Option) (*Server, string) {
	t.Helper()
	s, err := Listen("tcp4", "127.0.0.1:0", h, opts...)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve() }()
	t.Cleanup(func() {
		s.Close()
		select {
		case err := <-served:
			if !errors.Is(err, ErrServerClosed) {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after Close")
		}
	})
	return s, s.Addr().String()
}

func TestServerEcho(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    *apiEcho
		opts []Option
	}{
		{"default", &apiEcho{}, []Option{WithRings(2), WithBuffers(64, 2048)}},
		{"incremental-split", &apiEcho{split: true}, []Option{WithRings(2), WithBuffers(32, 64<<10), WithIncrementalBuffers()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, addr := serve(t, tc.h, tc.opts...)
			runEchoClients(t, addr, 32, []int{1, 100, 2048, 2049, 65536, 1 << 20})
		})
	}
}

func TestServerCloseReasons(t *testing.T) {
	h := &closeOnQuit{}
	s, addr := serve(t, h, WithRings(1), WithIdleTimeout(300*time.Millisecond))

	dial := func() net.Conn {
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	peer := dial()
	peer.Write([]byte("hi"))
	readN(t, peer, 2)
	peer.Close() // 对端关闭 -> io.EOF

	quit := dial()
	quit.Write([]byte("quit")) // 服务端 Conn.Close -> nil
	if b, _ := io.ReadAll(quit); len(b) != 0 {
		t.Fatalf("unexpected data %q", b)
	}
	quit.Close()

	idle := dial() // 空闲超时 -> ErrIdleTimeout
	defer idle.Close()
	idle.SetReadDeadline(time.Now().Add(5 * time.Second))
	idle.Read(make([]byte, 1))

	waitFor(t, func() bool { return len(h.closeErrs()) == 3 }, "three OnClose calls")
	got := h.closeErrs()
	want := []error{io.EOF, nil, ErrIdleTimeout}
	for i := range want {
		if !errors.Is(got[i], want[i]) && got[i] != want[i] {
			t.Fatalf("OnClose errors = %v, want %v", got, want)
		}
	}

	open := dial() // 关闭服务器 -> ErrServerClosed
	defer open.Close()
	waitFor(t, func() bool { return s.Connections() == 1 }, "connection open")
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.closeErrs(); len(got) != 4 || got[3] != ErrServerClosed {
		t.Fatalf("OnClose errors after Shutdown = %v", got)
	}
}

// closeOnQuit 回显数据，收到 "quit" 时关闭连接
type closeOnQuit struct{ apiEcho }

func (h *closeOnQuit) OnTraffic(c *Conn, data []byte) Action {
	if string(data) == "quit" {
		return Close
	}
	c.Write(data)
	return None
}

func readN(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	return b
}

// bigReply 收到任何数据都回复 payload
type bigReply struct {
	apiEcho
	payload []byte
}

func (h *bigReply) OnTraffic(c *Conn, data []byte) Action {
	c.Write(h.payload)
	return None
}

// 优雅关闭：已经写入的数据全部发出后才关闭连接，空闲连接立即关闭，不再接受新连接
func TestServerGracefulShutdown(t *testing.T) {
	payload := make([]byte, 8<<20)
	rand.Read(payload)
	h := &bigReply{payload: payload}
	s, addr := serve(t, h, WithRings(2))

	busy, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	idle, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	busy.Write([]byte("go"))
	waitFor(t, func() bool { return s.Connections() == 2 }, "two connections")
	time.Sleep(50 * time.Millisecond) // 让服务端把响应排进发送队列

	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr <- s.Shutdown(ctx)
	}()

	idle.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := idle.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("idle connection: n=%d err=%v, want closed", n, err)
	}
	time.Sleep(100 * time.Millisecond) // 关闭已经开始、对端还没读完
	if _, err := net.DialTimeout("tcp4", addr, 500*time.Millisecond); err == nil {
		t.Fatal("new connection accepted during shutdown")
	}

	busy.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(busy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("received %d bytes, want the full %d byte reply", len(got), len(payload))
	}
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for _, err := range h.closeErrs() {
		if err != ErrServerClosed {
			t.Fatalf("OnClose error %v, want ErrServerClosed", err)
		}
	}
}

// 对端不读时，优雅关闭在 ctx 到期后强制关闭并返回 ctx 的错误
func TestServerShutdownDeadline(t *testing.T) {
	h := &bigReply{payload: make([]byte, 32<<20)}
	s, addr := serve(t, h, WithRings(1), WithMaxWriteBuffer(-1))
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("go"))
	waitFor(t, func() bool { return s.Connections() == 1 }, "connection")
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Shutdown took %v", d)
	}
	if s.Connections() != 0 {
		t.Fatalf("connections after forced shutdown: %d", s.Connections())
	}
}

// AsyncWrite / AsyncClose 可以在其他 goroutine 中调用
func TestConnAsyncWrite(t *testing.T) {
	h := &apiEcho{opened: make(chan *Conn, 1)}
	_, addr := serve(t, h, WithRings(1))
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var sc *Conn
	select {
	case sc = <-h.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("no OnOpen")
	}

	done := make(chan error, 1)
	if err := sc.AsyncWrite([]byte("pushed"), func(err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := readN(t, c, 6); string(got) != "pushed" {
		t.Fatalf("got %q", got)
	}
	if err := sc.AsyncClose(); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("after AsyncClose: n=%d err=%v", n, err)
	}
	waitFor(t, func() bool { return len(h.closeErrs()) == 1 }, "OnClose")
	if err := h.closeErrs()[0]; err != nil {
		t.Fatalf("OnClose error %v, want nil", err)
	}
	// 关闭后的 AsyncWrite 通过 done 报告 ErrConnClosed
	sc.AsyncWrite([]byte("late"), func(err error) { done <- err })
	if err := <-done; err != ErrConnClosed {
		t.Fatalf("AsyncWrite after close: %v", err)
	}
}

// 端口为 0 且每个 ring 有自己的监听 socket 时，所有 ring 必须绑定同一个端口
func TestListenPortZeroReusePort(t *testing.T) {
	h := &apiEcho{}
	s, addr := serve(t, h, WithRings(4), WithReusePort(true))
	var conns []net.Conn
	for i := 0; i < 64; i++ {
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	waitFor(t, func() bool { return s.Connections() == 64 }, "64 connections")
	used := 0
	for _, rn := range s.rings {
		if rn.Connections() > 0 {
			used++
		}
	}
	if used < 2 {
		t.Fatalf("connections spread over %d rings, want several", used)
	}
	for _, c := range conns {
		c.Close()
	}
}

// 日志：空闲超时关闭连接时输出 debug 日志
func TestServerLogger(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := &apiEcho{}
	_, addr := serve(t, h, WithRings(1), WithIdleTimeout(100*time.Millisecond), WithLogger(logger))
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, func() bool { return bytes.Contains(buf.Bytes(), []byte("closing idle connection")) }, "idle timeout log")
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

// 对端发完数据后半关闭（只关写方向）：服务端仍要把回复发完再关闭
func TestPeerHalfClose(t *testing.T) {
	h := &apiEcho{}
	_, addr := serve(t, h, WithRings(1))
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := make([]byte, 256<<10)
	rand.Read(msg)
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	c.(*net.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %d bytes back after half-close, want %d", len(got), len(msg))
	}
	waitFor(t, func() bool { return len(h.closeErrs()) == 1 }, "OnClose")
	if err := h.closeErrs()[0]; err != io.EOF {
		t.Fatalf("OnClose error %v, want io.EOF", err)
	}
}
