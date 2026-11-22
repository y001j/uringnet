//go:build linux
// +build linux

package uringnet

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	socket "github.com/y001j/uringnet/sockets"
)

type testHandler struct {
	BuiltinEventEngine
	opened, closed atomic.Int64
	traffic        func(d *UserData) Action
}

func (h *testHandler) OnOpen(d *UserData) ([]byte, Action) {
	h.opened.Add(1)
	return nil, None
}

func (h *testHandler) OnClose(d UserData) Action {
	h.closed.Add(1)
	return None
}

func (h *testHandler) OnTraffic(d *UserData, _ *URingNet) Action {
	if h.traffic != nil {
		return h.traffic(d)
	}
	d.WriteBuf = d.Buffer
	return Echo
}

func freePort(t testing.TB) string {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func startServer(t testing.TB, h EventHandler, rings int, reusePort bool) (*Ringloop, string) {
	return startServerBufs(t, h, rings, reusePort, 256)
}

func startServerBufs(t testing.TB, h EventHandler, rings int, reusePort bool, bufs int) (*Ringloop, string) {
	return startServerCfg(t, h, rings, reusePort, bufs, nil)
}

func startServerCfg(t testing.TB, h EventHandler, rings int, reusePort bool, bufs int, cfg func(*URingNet)) (*Ringloop, string) {
	addr := freePort(t)
	opts := socket.SocketOptions{TCPNoDelay: socket.TCPNoDelay, ReusePort: reusePort}
	rns, err := NewMany(NetAddress{AddrType: socket.Tcp4, Address: addr}, 1024, false, rings, opts, h)
	if err != nil {
		t.Fatal(err)
	}
	for _, rn := range rns {
		rn.BufferSize = 2048
		if cfg != nil {
			cfg(rn)
		}
	}
	loop, err := SetLoops(rns, bufs)
	if err != nil {
		t.Fatal(err)
	}
	loop.RunMany()
	return loop, addr
}

func TestEchoConcurrent(t *testing.T) {
	h := &testHandler{}
	loop, addr := startServer(t, h, 4, true)
	defer loop.Shutdown()

	sizes := []int{1, 100, 2048, 2049, 65536, 1 << 20}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.Dial("tcp4", addr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			for _, n := range sizes {
				msg := make([]byte, n)
				rand.Read(msg)
				// Write and read concurrently so large messages cannot deadlock with both sides' buffers full
				go c.Write(msg)
				got := make([]byte, n)
				c.SetReadDeadline(time.Now().Add(10 * time.Second))
				if _, err := io.ReadFull(c, got); err != nil {
					errs <- fmt.Errorf("conn %d size %d: %w", i, n, err)
					return
				}
				if !bytes.Equal(got, msg) {
					errs <- fmt.Errorf("conn %d size %d: data mismatch", i, n)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.closed.Load() == 64 }, "OnClose for all connections")
	if h.opened.Load() != 64 {
		t.Fatalf("opened = %d", h.opened.Load())
	}
}

// Far fewer buffers than connections: in-place sends hold buffers and cause -ENOBUFS; data must stay intact across pause/resume.
func TestBufferStarvation(t *testing.T) {
	h := &testHandler{}
	loop, addr := startServerBufs(t, h, 1, false, 4)
	defer loop.Shutdown()

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.Dial("tcp4", addr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			msg := make([]byte, 256<<10)
			rand.Read(msg)
			go c.Write(msg)
			got := make([]byte, len(msg))
			c.SetReadDeadline(time.Now().Add(20 * time.Second))
			if _, err := io.ReadFull(c, got); err != nil {
				errs <- fmt.Errorf("conn %d: %w", i, err)
				return
			}
			if !bytes.Equal(got, msg) {
				errs <- fmt.Errorf("conn %d: data mismatch", i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// The client does not read while the server keeps writing: in-flight and pending sends, short writes and closing must not leak.
func TestPeerCloseDuringSend(t *testing.T) {
	big := make([]byte, 256<<10)
	h := &testHandler{traffic: func(d *UserData) Action {
		d.WriteBuf = big
		return Write
	}}
	loop, addr := startServer(t, h, 1, false)
	defer loop.Shutdown()
	for i := 0; i < 20; i++ {
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 10; j++ {
			c.Write([]byte("x"))
		}
		time.Sleep(5 * time.Millisecond)
		c.Close()
	}
	waitFor(t, func() bool { return h.closed.Load() == 20 }, "OnClose after peer close")
	if n := loop.RingNet[0].Connections(); n != 0 {
		t.Fatalf("connections = %d", n)
	}
}

func TestEchoAndClose(t *testing.T) {
	h := &testHandler{traffic: func(d *UserData) Action {
		d.WriteBuf = append(d.WriteBuf, "bye:"...)
		d.WriteBuf = append(d.WriteBuf, d.Buffer...)
		return EchoAndClose
	}}
	loop, addr := startServer(t, h, 2, false)
	defer loop.Shutdown()

	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hello"))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "bye:hello" {
		t.Fatalf("got %q", got)
	}
	waitFor(t, func() bool { return h.closed.Load() == 1 }, "OnClose")
}

func TestCloseAction(t *testing.T) {
	h := &testHandler{traffic: func(d *UserData) Action { return Close }}
	loop, addr := startServer(t, h, 1, false)
	defer loop.Shutdown()

	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("x"))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("expected EOF, got n=%d err=%v", n, err)
	}
	waitFor(t, func() bool { return h.closed.Load() == 1 }, "OnClose")
}

// After WriteBuf = Buffer the package must drop WriteBuf, so the next append cannot write into a returned receive buffer.
func TestWriteBufAliasReset(t *testing.T) {
	var aliasLeft atomic.Bool
	h := &testHandler{}
	h.traffic = func(d *UserData) Action {
		if cap(d.WriteBuf) != 0 && overlaps(d.WriteBuf, d.Buffer) {
			aliasLeft.Store(true)
		}
		d.WriteBuf = d.Buffer
		return Echo
	}
	loop, addr := startServer(t, h, 1, false)
	defer loop.Shutdown()
	c, _ := net.Dial("tcp4", addr)
	defer c.Close()
	buf := make([]byte, 4)
	for i := 0; i < 50; i++ {
		c.Write([]byte("ping"))
		io.ReadFull(c, buf)
	}
	if aliasLeft.Load() {
		t.Fatal("WriteBuf still aliases the receive buffer")
	}
}

func TestShutdown(t *testing.T) {
	h := &testHandler{}
	loop, addr := startServer(t, h, 2, false)
	var conns []net.Conn
	for i := 0; i < 10; i++ {
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	waitFor(t, func() bool { return h.opened.Load() == 10 }, "OnOpen")
	loop.Shutdown()
	loop.Wait()
	if h.closed.Load() != 10 {
		t.Fatalf("closed = %d", h.closed.Load())
	}
	for _, c := range conns {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("expected connection closed")
		}
		c.Close()
	}
	if _, err := net.DialTimeout("tcp4", addr, time.Second); err == nil {
		t.Fatal("listener still open after Shutdown")
	}
}

// runEchoClients: several connections send random data concurrently and check the echo.
func runEchoClients(t *testing.T, addr string, conns int, sizes []int) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, conns)
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.Dial("tcp4", addr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			for _, n := range sizes {
				msg := make([]byte, n)
				rand.Read(msg)
				go c.Write(msg)
				got := make([]byte, n)
				c.SetReadDeadline(time.Now().Add(10 * time.Second))
				if _, err := io.ReadFull(c, got); err != nil {
					errs <- fmt.Errorf("conn %d size %d: %w", i, n, err)
					return
				}
				if !bytes.Equal(got, msg) {
					errs <- fmt.Errorf("conn %d size %d: data mismatch", i, n)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// Zero-copy send: in-place sends of receive buffers use SEND_ZC (fixed buffers); buffers return only after the notification.
func TestZeroCopyEcho(t *testing.T) {
	h := &testHandler{}
	loop, addr := startServerCfg(t, h, 2, false, 64, func(rn *URingNet) { rn.ZeroCopyThreshold = 512 })
	defer loop.Shutdown()

	runEchoClients(t, addr, 16, []int{100, 1500, 2048, 4096, 65536, 1 << 20})
	if loop.RingNet[0].backend == BackendEpoll {
		t.Skip("the epoll backend has no zero-copy send; echo data was verified above")
	}

	total := 0
	for _, rn := range loop.RingNet {
		res := make(chan [2]int, 1)
		rn.Post(func() { res <- [2]int{rn.zcSends, rn.fixedIdx} })
		r := <-res
		t.Logf("ring %d: zcSends=%d fixedIdx=%d zcOff=%v", rn.index, r[0], r[1], rn.zcOff)
		total += r[0]
	}
	if total == 0 {
		t.Error("SEND_ZC was never used")
	}
	waitFor(t, func() bool { return h.closed.Load() == 16 }, "OnClose")
	// Every held receive buffer has been returned (all notification CQEs handled)
	for _, rn := range loop.RingNet {
		waitFor(t, func() bool {
			res := make(chan int, 1)
			rn.Post(func() { res <- rn.held })
			return <-res == 0
		}, "held buffers released")
	}
}

// WriteNoCopy: data goes out in order (before WriteBuf of the same callback), and release is called exactly once per buffer.
func TestWriteNoCopy(t *testing.T) {
	big := make([]byte, 300<<10)
	rand.Read(big)
	var released atomic.Int64
	h := &testHandler{}
	h.traffic = func(d *UserData) Action {
		if err := d.WriteNoCopy(big, func() { released.Add(1) }); err != nil {
			t.Error(err)
		}
		d.WriteBuf = append(d.WriteBuf, "END"...)
		return Write
	}
	for _, zc := range []int{0, 16 << 10} {
		released.Store(0)
		loop, addr := startServerCfg(t, h, 1, false, 64, func(rn *URingNet) { rn.ZeroCopyThreshold = zc })
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			t.Fatal(err)
		}
		want := append(append([]byte(nil), big...), "END"...)
		for i := 0; i < 5; i++ {
			c.Write([]byte("x")) // 1 byte: TCP is a stream, so a longer write could arrive as several OnTraffic calls
			got := make([]byte, len(want))
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(c, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("zc=%d round %d: data mismatch", zc, i)
			}
		}
		c.Close()
		waitFor(t, func() bool { return released.Load() == 5 }, fmt.Sprintf("release x5 (zc=%d), got %d", zc, released.Load()))
		loop.Shutdown()
	}
}

func TestSpinTimeEcho(t *testing.T) {
	h := &testHandler{}
	loop, addr := startServerCfg(t, h, 2, false, 64, func(rn *URingNet) { rn.SpinTime = 50 * time.Microsecond })
	runEchoClients(t, addr, 16, []int{1, 128, 4096, 65536})
	loop.Shutdown()
	loop.Wait()
	if h.closed.Load() != 16 {
		t.Fatalf("closed = %d", h.closed.Load())
	}
}

// Gather send: a large WriteNoCopy chunk and a small WriteBuf tail cannot be merged and go out in one sendmsg.
func TestGatherSend(t *testing.T) {
	big := make([]byte, 64<<10)
	rand.Read(big)
	h := &testHandler{}
	h.traffic = func(d *UserData) Action {
		d.WriteNoCopy(big, nil)
		d.WriteBuf = append(d.WriteBuf, "END"...)
		return Write
	}
	loop, addr := startServerCfg(t, h, 1, false, 64, nil)
	defer loop.Shutdown()
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	want := append(append([]byte(nil), big...), "END"...)
	for i := 0; i < 20; i++ {
		c.Write([]byte("x"))
		got := make([]byte, len(want))
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("round %d: data mismatch", i)
		}
	}
	rn := loop.RingNet[0]
	res := make(chan int, 1)
	rn.Post(func() { res <- rn.gatherSends })
	n := <-res
	t.Logf("gather sends: %d", n)
	if n == 0 {
		t.Error("sendmsg gather send was never used")
	}
}

// Recv bundles are tested only on kernels with the data-corruption fixes (6.16+).
func TestRecvBundleEcho(t *testing.T) {
	if !bundleSafeKernel() {
		t.Skip("recv bundle needs Linux 6.16+ (earlier kernels have known data-gap bugs)")
	}
	h := &testHandler{}
	loop, addr := startServerCfg(t, h, 2, false, 64, func(rn *URingNet) { rn.RecvBundle = BundleOn })
	defer loop.Shutdown()
	if !loop.RingNet[0].bundle {
		t.Skip("kernel does not support recv bundles")
	}
	runEchoClients(t, addr, 32, []int{1, 100, 2048, 2049, 65536, 1 << 20})
}

// Incremental buffer ring: a few large buffers, data of several connections interleaved, several slices sent in place.
func TestIncrementalBuffersEcho(t *testing.T) {
	h := &testHandler{}
	loop, addr := startServerCfg(t, h, 2, false, 8, func(rn *URingNet) {
		rn.IncrementalBuffers = true
	})
	defer loop.Shutdown()
	if !loop.RingNet[0].inc {
		t.Skip("kernel does not support incremental buffer rings (needs 6.12+)")
	}
	runEchoClients(t, addr, 32, []int{1, 100, 2048, 2049, 65536, 1 << 20})
	waitFor(t, func() bool { return h.closed.Load() == 32 }, "OnClose x32")
	for _, rn := range loop.RingNet {
		waitFor(t, func() bool {
			res := make(chan [2]int, 1)
			rn.Post(func() { res <- [2]int{rn.held, rn.incPinned} })
			r := <-res
			return r[0] == 0 && r[1] == 0
		}, "all buffer references released")
	}
}

// Incremental: with few large buffers and many connections the buffers run out (-ENOBUFS) while connections close at random.
// Regression test: connections waiting for buffers were once never woken and the server hung.
func TestIncrementalBuffersNoStall(t *testing.T) {
	h := &testHandler{}
	loop, addr := startServerCfg(t, h, 1, false, 8, func(rn *URingNet) {
		rn.IncrementalBuffers = true
		rn.BufferSize = 128 << 10
	})
	defer loop.Shutdown()
	rn := loop.RingNet[0]
	if !rn.inc {
		t.Skip("kernel does not support incremental buffer rings (needs 6.12+)")
	}
	for _, ph := range [][3]int{{512, 1, 16384}, {512, 8, 16384}, {64, 8, 16384}} {
		conns, depth, size := ph[0], ph[1], ph[2]
		var wg sync.WaitGroup
		var ops atomic.Int64
		stop := make(chan struct{})
		for i := 0; i < conns; i++ {
			c, err := net.Dial("tcp4", addr)
			if err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				msg := make([]byte, size*depth)
				buf := make([]byte, size*depth)
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := c.Write(msg); err != nil {
						return
					}
					c.SetReadDeadline(time.Now().Add(2 * time.Second))
					if _, err := io.ReadFull(c, buf); err != nil {
						return
					}
					ops.Add(1)
				}
			}(c)
		}
		var last int64
		for i := 0; i < 4; i++ {
			time.Sleep(250 * time.Millisecond)
			if n := ops.Load(); n == last && i > 0 {
				t.Fatalf("phase %v stalled after %d ops", ph, n)
			} else {
				last = n
			}
		}
		close(stop) // close at an arbitrary moment, possibly with data in flight
		wg.Wait()
		waitFor(t, func() bool {
			res := make(chan [4]int, 1)
			rn.Post(func() { res <- [4]int{len(rn.conns), rn.held, rn.incPinned, len(rn.starved)} })
			r := <-res
			return r == [4]int{}
		}, fmt.Sprintf("clean state after phase %v", ph))
	}
}

// Incremental buffers + zero-copy send (uringnet-best-zc in the cross-machine benchmark): slices sent in place use SEND_ZC,
// and their references are released only after the notification CQE.
func TestIncrementalBuffersZeroCopy(t *testing.T) {
	h := &testHandler{}
	loop, addr := startServerCfg(t, h, 2, false, 32, func(rn *URingNet) {
		rn.IncrementalBuffers = true
		rn.BufferSize = 64 << 10
		rn.ZeroCopyThreshold = 1024
	})
	defer loop.Shutdown()
	if !loop.RingNet[0].inc {
		t.Skip("kernel does not support incremental buffer rings (needs 6.12+)")
	}
	runEchoClients(t, addr, 32, []int{100, 2048, 65536, 1 << 20})
	waitFor(t, func() bool { return h.closed.Load() == 32 }, "OnClose x32")
	total := 0
	for _, rn := range loop.RingNet {
		waitFor(t, func() bool {
			res := make(chan [2]int, 1)
			rn.Post(func() { res <- [2]int{rn.held, rn.incPinned} })
			r := <-res
			return r[0] == 0 && r[1] == 0
		}, "all buffer references released after notifications")
		res := make(chan int, 1)
		rn.Post(func() { res <- rn.zcSends })
		total += <-res
	}
	if total == 0 {
		t.Error("SEND_ZC was never used")
	}
}

func TestUnsupportedProtocol(t *testing.T) {
	if _, err := NewMany(NetAddress{AddrType: socket.Udp4, Address: "127.0.0.1:0"}, 64, false, 1, socket.SocketOptions{}, &testHandler{}); err == nil {
		t.Fatal("expected error for udp")
	}
}

func waitFor(t testing.TB, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- benchmarks: c persistent connections, ping-pong ----

func benchPingPong(b *testing.B, addr string, conns, size int) {
	msg := make([]byte, size)
	var ops atomic.Int64
	var wg sync.WaitGroup
	per := b.N/conns + 1
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < conns; i++ {
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			b.Fatal(err)
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			defer c.Close()
			buf := make([]byte, size)
			for j := 0; j < per; j++ {
				if _, err := c.Write(msg); err != nil {
					return
				}
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				ops.Add(1)
			}
		}(c)
	}
	wg.Wait()
	b.ReportMetric(float64(ops.Load())/b.Elapsed().Seconds(), "req/s")
}

func stdEchoServer(b *testing.B) (string, func()) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 4096)
				io.CopyBuffer(c, c, buf)
				c.Close()
			}()
		}
	}()
	return l.Addr().String(), func() { l.Close() }
}

func BenchmarkEcho(b *testing.B) {
	for _, conns := range []int{1, 64, 512} {
		for _, size := range []int{128, 16384} {
			b.Run(fmt.Sprintf("uringnet/c%d/%dB", conns, size), func(b *testing.B) {
				h := &testHandler{}
				addr := freePort(b)
				opts := socket.SocketOptions{TCPNoDelay: socket.TCPNoDelay, ReusePort: true}
				rns, err := NewMany(NetAddress{AddrType: socket.Tcp4, Address: addr}, 4096, false, 4, opts, h)
				if err != nil {
					b.Fatal(err)
				}
				loop, err := SetLoops(rns, 4096)
				if err != nil {
					b.Fatal(err)
				}
				loop.RunMany()
				defer loop.Shutdown()
				benchPingPong(b, addr, conns, size)
			})
			b.Run(fmt.Sprintf("std/c%d/%dB", conns, size), func(b *testing.B) {
				addr, stop := stdEchoServer(b)
				defer stop()
				benchPingPong(b, addr, conns, size)
			})
		}
	}
}
