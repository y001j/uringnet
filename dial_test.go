//go:build linux
// +build linux

package uringnet

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// collector 记录拨出连接收到的数据和 OnOpen 时看到的 Context
type collector struct {
	BuiltinHandler
	mu       sync.Mutex
	got      bytes.Buffer
	openCtx  []interface{}
	closeErr []error
}

func (h *collector) OnOpen(c *Conn) Action {
	h.mu.Lock()
	h.openCtx = append(h.openCtx, c.Context())
	h.mu.Unlock()
	return None
}

func (h *collector) OnTraffic(c *Conn, data []byte) Action {
	h.mu.Lock()
	h.got.Write(data)
	h.mu.Unlock()
	return None
}

func (h *collector) OnClose(c *Conn, err error) {
	h.mu.Lock()
	h.closeErr = append(h.closeErr, err)
	h.mu.Unlock()
}

func (h *collector) received() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.got.String()
}

func newTestClient(t *testing.T, h Handler, opts ...Option) *Client {
	t.Helper()
	cl, err := NewClient(h, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl
}

func TestDialEcho(t *testing.T) {
	_, addr := serve(t, &apiEcho{}, WithRings(1))
	h := &collector{}
	cl := newTestClient(t, h, WithRings(2))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := cl.Dial(ctx, "tcp4", addr, "upstream")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AsyncWrite([]byte("ping"), nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.received() == "ping" }, "echo over a dialed connection")
	h.mu.Lock()
	if len(h.openCtx) != 1 || h.openCtx[0] != "upstream" {
		t.Fatalf("OnOpen saw Context %v, want [upstream]", h.openCtx)
	}
	h.mu.Unlock()
	if st := cl.Stats(); st.Dialed != 1 || st.Connections != 1 {
		t.Fatalf("client stats %+v", st)
	}
	c.AsyncClose()
	waitFor(t, func() bool { return cl.Connections() == 0 }, "dialed connection closed")
}

func TestDialRefused(t *testing.T) {
	cl := newTestClient(t, &collector{}, WithRings(1))
	addr := freePort(t) // 已经关闭的端口
	_, err := cl.Dial(context.Background(), "tcp4", addr, nil)
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("Dial to a closed port: %v, want ECONNREFUSED", err)
	}
	done := make(chan error, 1)
	if err := cl.DialAsync("tcp4", addr, nil, func(c *Conn, err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("DialAsync to a closed port: %v", err)
	}
	// 有些环境的 DNS（如代理软件的 fake-IP）会把任何名字都解析成地址，这时跳过这项检查
	if _, err := net.LookupHost("no-such-host.invalid"); err != nil {
		if _, err := cl.Dial(context.Background(), "tcp4", "no-such-host.invalid:80", nil); err == nil {
			t.Fatal("Dial to an unresolvable host succeeded")
		}
	}
}

// ctx 到期时放弃拨号：返回 ctx 的错误，之后也不会留下连接
func TestDialContextTimeout(t *testing.T) {
	h := &collector{}
	cl := newTestClient(t, h, WithRings(1))
	// 不可路由的地址：SYN 没有回应，连接一直挂着
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := cl.Dial(ctx, "tcp4", "10.255.255.1:9", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
			t.Skipf("no route to the test address in this environment: %v", err)
		}
		t.Fatalf("Dial = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Dial returned after %v", d)
	}
	time.Sleep(100 * time.Millisecond)
	if n := cl.Connections(); n != 0 {
		t.Fatalf("%d connections after an abandoned dial", n)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.openCtx) != 0 {
		t.Fatal("OnOpen called for an abandoned dial")
	}
}

func TestDialUnix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "echo.sock")
	s, err := Listen("unix", path, &apiEcho{}, WithRings(1))
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	defer s.Close()

	h := &collector{}
	cl := newTestClient(t, h, WithRings(1))
	c, err := cl.Dial(context.Background(), "unix", path, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.AsyncWrite([]byte("over unix"), nil)
	waitFor(t, func() bool { return h.received() == "over unix" }, "echo over a unix socket")
}

// proxy：下游连接接入后从同一个事件循环拨上游，两边在回调中直接互相写
type proxy struct {
	BuiltinHandler
	upstream string
}

type proxySide struct {
	peer    *Conn
	pending []byte // 上游连接建立前收到的下游数据
}

func (p *proxy) OnOpen(c *Conn) Action {
	if c.Context() != nil { // 拨出的上游连接：Context 在 OnOpen 之前已经设置
		return None
	}
	down := &proxySide{}
	c.SetContext(down)
	c.DialAsync("tcp4", p.upstream, &proxySide{peer: c}, func(up *Conn, err error) {
		if err != nil {
			c.Close()
			return
		}
		down.peer = up
		if len(down.pending) > 0 {
			up.Write(down.pending)
			down.pending = nil
		}
	})
	return None
}

func (p *proxy) OnTraffic(c *Conn, data []byte) Action {
	side := c.Context().(*proxySide)
	if side.peer == nil {
		side.pending = append(side.pending, data...)
		return None
	}
	side.peer.Write(data) // 同一个事件循环：直接写，数据被拷贝进对方的发送队列
	return None
}

func (p *proxy) OnClose(c *Conn, err error) {
	if side, ok := c.Context().(*proxySide); ok && side.peer != nil {
		side.peer.Close()
	}
}

func TestDialProxy(t *testing.T) {
	_, upstream := serve(t, &apiEcho{}, WithRings(2))
	front, addr := serve(t, &proxy{upstream: upstream}, WithRings(2))

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp4", addr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			msg := make([]byte, 1<<20)
			rand.Read(msg)
			go c.Write(msg)
			got := make([]byte, len(msg))
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(c, got); err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, msg) {
				errs <- errors.New("data differs through the proxy")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// 客户端关闭后，代理两侧的连接都要关闭
	waitFor(t, func() bool { return front.Connections() == 0 }, "proxy connections closed")
	if st := front.Stats(); st.Accepted != 8 || st.Dialed != 8 {
		t.Fatalf("proxy stats %+v", st)
	}
}
