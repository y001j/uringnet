//go:build linux
// +build linux

package uringnet

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// onLoop runs fn on rn's event loop thread and waits for it (to read fields owned by the loop thread).
func onLoop(t testing.TB, rn *URingNet, fn func()) {
	t.Helper()
	done := make(chan struct{})
	if err := rn.Post(func() { fn(); close(done) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event loop did not run the posted function")
	}
}

// When fds run out (EMFILE) the multishot accept must not spin: it re-arms with a doubling wait and accepts again once fds are free.
func TestAcceptBackoffOnEMFILE(t *testing.T) {
	// Lower the fd limit before starting the server: the kernel records RLIMIT_NOFILE when the accept is submitted,
	// and the multishot accept is submitted only once, so lowering the limit later would not affect it
	var old unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &old); err != nil {
		t.Fatal(err)
	}
	defer unix.Setrlimit(unix.RLIMIT_NOFILE, &old)
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	lim := unix.Rlimit{Cur: uint64(len(entries) + 64), Max: old.Max}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}

	h := &testHandler{}
	loop, addr := startServer(t, h, 1, false)
	defer loop.Shutdown()
	rn := loop.RingNet[0]
	onLoop(t, rn, func() {}) // make sure the accept has been submitted

	// Create the client socket first: once the fds are used up, connect needs no new fd while the server's accept fails with EMFILE
	cfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	sa := &unix.SockaddrInet4{Port: tcpAddr.Port}
	copy(sa.Addr[:], tcpAddr.IP.To4())

	// Use up the fds, and keep grabbing any that get freed meanwhile (the runtime or earlier tests may close fds
	// asynchronously), so the server's accept never gets one
	var mu sync.Mutex
	var fillers []int
	fill := func() {
		mu.Lock()
		defer mu.Unlock()
		for {
			fd, err := unix.Dup(0)
			if err != nil {
				return
			}
			fillers = append(fillers, fd)
		}
	}
	release := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, fd := range fillers {
			unix.Close(fd)
		}
		fillers = nil
	}
	defer release()
	fill()
	stop := make(chan struct{})
	absorbed := make(chan struct{})
	go func() {
		defer close(absorbed)
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Microsecond):
				fill()
			}
		}
	}()

	var before int
	onLoop(t, rn, func() { before = rn.acceptArms })
	if err := unix.Connect(cfd, sa); err != nil { // the kernel completes the handshake; the connection waits in the backlog
		close(stop)
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	var arms int
	onLoop(t, rn, func() { arms = rn.acceptArms - before })
	close(stop)
	<-absorbed
	// Backoff 5, 10, 20, 40, 80, 160ms: about 6 re-arms in 400ms; without backoff it is many thousands
	if arms < 1 || arms > 12 {
		t.Fatalf("accept re-armed %d times in 400ms during EMFILE, want a few", arms)
	}
	if h.opened.Load() != 0 {
		t.Fatalf("connection accepted while out of fds")
	}

	release()
	unix.Setrlimit(unix.RLIMIT_NOFILE, &old)
	waitFor(t, func() bool { return h.opened.Load() == 1 }, "accept after fds are released")

	f := os.NewFile(uintptr(cfd), "client")
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := []byte("ping")
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := c.Read(got); err != nil || string(got) != "ping" {
		t.Fatalf("echo after recovery: %q %v", got, err)
	}
}

// A peer that only writes: above MaxWriteBuffer the server stops receiving, so what it buffers is bounded;
// once the peer reads, receiving resumes and the data arrives intact.
func TestBackpressureBoundsQueue(t *testing.T) {
	const max = 64 << 10
	h := &testHandler{}
	loop, addr := startServerCfg(t, h, 1, false, 256, func(rn *URingNet) { rn.MaxWriteBuffer = max })
	defer loop.Shutdown()
	rn := loop.RingNet[0]

	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	payload := make([]byte, 32<<20)
	rand.Read(payload)
	writeErr := make(chan error, 1)
	go func() {
		_, err := c.Write(payload)
		writeErr <- err
	}()

	// Wait until the server pauses and TCP flow control blocks the client, then look at what the server buffers
	waitFor(t, func() bool {
		var paused bool
		onLoop(t, rn, func() {
			for _, cc := range rn.conns {
				paused = cc.paused
			}
		})
		return paused
	}, "receive paused by backpressure")
	time.Sleep(300 * time.Millisecond)
	var queued, pauses int
	onLoop(t, rn, func() {
		for _, cc := range rn.conns {
			queued = cc.qBytes
		}
		pauses = rn.pauses
	})
	// The limit plus the last batch before the pause took effect (buffers received before the cancellation completed)
	if queued > max+256*2048 {
		t.Fatalf("queued %d bytes with MaxWriteBuffer %d", queued, max)
	}
	select {
	case err := <-writeErr:
		t.Fatalf("client finished writing 32MB while the server should have stopped reading (err %v)", err)
	default:
	}

	got := make([]byte, len(payload))
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("echoed data differs")
	}
	t.Logf("queued %d bytes while paused; %d pauses", queued, pauses)
}

// IdleTimeout: a connection with no traffic is closed after the timeout; an active one is not.
func TestIdleTimeout(t *testing.T) {
	const idle = 200 * time.Millisecond
	h := &testHandler{}
	loop, addr := startServerCfg(t, h, 1, false, 256, func(rn *URingNet) { rn.IdleTimeout = idle })
	defer loop.Shutdown()

	quiet, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer quiet.Close()
	busy, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	// busy exchanges data every 50ms for three times the timeout
	busyErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 4)
		for end := time.Now().Add(3 * idle); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			busy.SetDeadline(time.Now().Add(time.Second))
			if _, err := busy.Write([]byte("ping")); err != nil {
				busyErr <- err
				return
			}
			if _, err := io.ReadFull(busy, buf); err != nil {
				busyErr <- err
				return
			}
		}
		busyErr <- nil
	}()

	start := time.Now()
	quiet.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := quiet.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("expected the idle connection to be closed, got n=%d err=%v", n, err)
	}
	elapsed := time.Since(start)
	// Closed within 1-1.25x the timeout; leave some room for scheduling
	if elapsed < idle || elapsed > 2*idle {
		t.Fatalf("idle connection closed after %v, want %v–%v", elapsed, idle, 2*idle)
	}
	if err := <-busyErr; err != nil {
		t.Fatalf("active connection was closed: %v", err)
	}
	t.Logf("idle connection closed after %v", elapsed)
}

// WriteTimeout: a peer that does not read is closed once the send queue makes no progress for that long.
func TestWriteTimeout(t *testing.T) {
	const wt = 200 * time.Millisecond
	big := make([]byte, 16<<20)
	h := &testHandler{traffic: func(d *UserData) Action {
		d.WriteBuf = big
		return Write
	}}
	loop, addr := startServerCfg(t, h, 1, false, 256, func(rn *URingNet) { rn.WriteTimeout = wt })
	defer loop.Shutdown()

	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("x")) // triggers a 16MB response that is never read
	start := time.Now()
	waitFor(t, func() bool { return h.closed.Load() == 1 }, "close by WriteTimeout")
	elapsed := time.Since(start)
	if elapsed < wt {
		t.Fatalf("closed after %v, before WriteTimeout %v", elapsed, wt)
	}
	var timeouts int
	onLoop(t, loop.RingNet[0], func() { timeouts = loop.RingNet[0].timeouts })
	if timeouts != 1 {
		t.Fatalf("timeouts = %d", timeouts)
	}
	t.Logf("stalled connection closed after %v", elapsed)
}
