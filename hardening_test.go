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

// onLoop 在 rn 的事件循环线程中执行 fn 并等待它完成（读取只在 Loop 线程中访问的字段）
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

// fd 用完（EMFILE）时 multishot accept 不能空转：等待时间翻倍地重新提交，fd 释放后恢复 accept
func TestAcceptBackoffOnEMFILE(t *testing.T) {
	// 先调低 fd 上限再启动服务端：内核在提交 accept 请求时记下当时的 RLIMIT_NOFILE，
	// multishot accept 只提交一次，之后调低上限对它不起作用
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
	onLoop(t, rn, func() {}) // 确保 accept 已经提交

	// 客户端 socket 先建好：占满 fd 之后 connect 不需要新的 fd，而服务端 accept 会 EMFILE
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

	// 占满 fd；等待期间持续抢占被释放的 fd（运行时或之前的测试可能异步关闭 fd），
	// 保证服务端的 accept 一直拿不到 fd
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
	if err := unix.Connect(cfd, sa); err != nil { // 握手由内核完成，连接留在监听队列中
		close(stop)
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	var arms int
	onLoop(t, rn, func() { arms = rn.acceptArms - before })
	close(stop)
	<-absorbed
	// 退避 5、10、20、40、80、160ms：400ms 内约 6 次；没有退避时是成千上万次
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

// 对端只发不读：发送队列超过 MaxWriteBuffer 时暂停接收，服务端缓存的数据有上限；
// 对端开始读取后恢复接收，数据完整无误
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

	// 等服务端暂停接收、客户端的发送被 TCP 流控挡住，然后看服务端缓存了多少
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
	// 上限 + 暂停生效前最后一批数据（取消完成前已收到的缓冲区）
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

// IdleTimeout：没有收发的连接在超时后被关闭；持续收发的连接不受影响
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

	// busy 每 50ms 收发一次，持续 3 倍超时时间
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
	// 超时后 1–1.25 倍时间内关闭；给调度留些余量
	if elapsed < idle || elapsed > 2*idle {
		t.Fatalf("idle connection closed after %v, want %v–%v", elapsed, idle, 2*idle)
	}
	if err := <-busyErr; err != nil {
		t.Fatalf("active connection was closed: %v", err)
	}
	t.Logf("idle connection closed after %v", elapsed)
}

// WriteTimeout：对端不读，发送队列长时间没有进展时关闭连接
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
	c.Write([]byte("x")) // 触发 16MB 的响应，但从不读取
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
