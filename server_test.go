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

// apiEcho echoes data; with split it writes each chunk back in two slices, to test several in-place sends from one buffer with incremental buffers.
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

// serve starts a Server and returns its address; at the end of the test it closes it and checks what Serve returned.
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
	peer.Close() // peer close -> io.EOF

	quit := dial()
	quit.Write([]byte("quit")) // server-side Conn.Close -> nil
	if b, _ := io.ReadAll(quit); len(b) != 0 {
		t.Fatalf("unexpected data %q", b)
	}
	quit.Close()

	idle := dial() // idle timeout -> ErrIdleTimeout
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

	open := dial() // server shutdown -> ErrServerClosed
	defer open.Close()
	waitFor(t, func() bool { return s.Connections() == 1 }, "connection open")
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.closeErrs(); len(got) != 4 || got[3] != ErrServerClosed {
		t.Fatalf("OnClose errors after Shutdown = %v", got)
	}
}

// closeOnQuit echoes data and closes the connection on "quit".
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

// bigReply answers any data with payload.
type bigReply struct {
	apiEcho
	payload []byte
}

func (h *bigReply) OnTraffic(c *Conn, data []byte) Action {
	c.Write(h.payload)
	return None
}

// Graceful shutdown: connections close only after the data written has been sent, idle ones at once, and no new connections are accepted.
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
	time.Sleep(50 * time.Millisecond) // let the server queue the response

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
	time.Sleep(100 * time.Millisecond) // shutdown has started, the peer has not read everything yet
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

// With a peer that does not read, a graceful shutdown forces the close when ctx ends and returns ctx's error.
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

// AsyncWrite / AsyncClose can be called from other goroutines.
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
	// AsyncWrite after the close reports ErrConnClosed through done
	sc.AsyncWrite([]byte("late"), func(err error) { done <- err })
	if err := <-done; err != ErrConnClosed {
		t.Fatalf("AsyncWrite after close: %v", err)
	}
}

// With port 0 and one listening socket per ring, all rings must bind the same port.
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

// Logging: closing an idle connection logs a debug message.
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

// The peer half-closes (shuts down its write side) after sending: the server must still send the reply before closing.
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

func TestServerStats(t *testing.T) {
	h := &apiEcho{}
	s, addr := serve(t, h, WithRings(2), WithIdleTimeout(200*time.Millisecond))
	msg := make([]byte, 1000)
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Write(msg)
		readN(t, c, len(msg))
		if i < 2 {
			c.Close()
		} else {
			defer c.Close() // left alone, closed by the idle timeout
		}
	}
	waitFor(t, func() bool { return s.Stats().IdleTimeouts == 1 }, "idle timeout counted")
	st := s.Stats()
	if st.Accepted != 3 || st.BytesRead != 3000 || st.BytesWritten != 3000 || st.Connections != 0 {
		t.Fatalf("stats = %+v", st)
	}
}
