//go:build linux
// +build linux

package uringnet

import (
	"net"
	"os"
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
	h := &testHandler{}
	loop, addr := startServer(t, h, 1, false)
	defer loop.Shutdown()
	rn := loop.RingNet[0]

	var old unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &old); err != nil {
		t.Fatal(err)
	}
	defer unix.Setrlimit(unix.RLIMIT_NOFILE, &old)
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	lim := unix.Rlimit{Cur: uint64(len(entries) + 32), Max: old.Max}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}

	// 占满 fd，只留一个给客户端的 socket：服务端 accept 时就会 EMFILE
	var fillers []int
	defer func() {
		for _, fd := range fillers {
			unix.Close(fd)
		}
	}()
	for {
		fd, err := unix.Dup(0)
		if err != nil {
			break
		}
		fillers = append(fillers, fd)
	}
	unix.Close(fillers[len(fillers)-1])
	fillers = fillers[:len(fillers)-1]

	var before int
	onLoop(t, rn, func() { before = rn.acceptArms })
	c, err := net.Dial("tcp4", addr) // 握手由内核完成，连接留在监听队列中
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	time.Sleep(400 * time.Millisecond)
	var arms int
	onLoop(t, rn, func() { arms = rn.acceptArms - before })
	// 退避 5、10、20、40、80、160ms：400ms 内约 6 次；没有退避时是成千上万次
	if arms < 1 || arms > 12 {
		t.Fatalf("accept re-armed %d times in 400ms during EMFILE, want a few", arms)
	}
	if h.opened.Load() != 0 {
		t.Fatalf("connection accepted while out of fds")
	}

	for _, fd := range fillers {
		unix.Close(fd)
	}
	fillers = nil
	unix.Setrlimit(unix.RLIMIT_NOFILE, &old)
	waitFor(t, func() bool { return h.opened.Load() == 1 }, "accept after fds are released")

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
