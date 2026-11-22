//go:build linux
// +build linux

package uringnet

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"syscall"
	"testing"
)

// Explicitly chosen epoll backend: TCP echo, dialing and UDP all work (independent of URINGNET_BACKEND).
func TestEpollBackend(t *testing.T) {
	s, addr := serve(t, &apiEcho{}, WithRings(2), WithBackend(BackendEpoll))
	if s.Backend() != BackendEpoll {
		t.Fatalf("Backend() = %v", s.Backend())
	}
	runEchoClients(t, addr, 8, []int{1, 2048, 65536, 1 << 20})

	h := &collector{}
	cl := newTestClient(t, h, WithRings(1), WithBackend(BackendEpoll))
	c, err := cl.Dial(context.Background(), "tcp4", addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.AsyncWrite([]byte("via epoll"), nil)
	waitFor(t, func() bool { return h.received() == "via epoll" }, "dial echo on epoll")

	ps := servePacket(t, "udp4", "127.0.0.1:0", udpEcho, WithRings(1), WithBackend(BackendEpoll))
	uc, err := net.Dial("udp4", ps.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	if got := udpRoundTrip(t, uc, []byte("udp via epoll")); string(got) != "udp via epoll" {
		t.Fatalf("got %q", got)
	}
}

func TestBackendSelection(t *testing.T) {
	// On the real path setupError adds a hint and the original errno is not in the error chain
	if !ioURingUnavailable(setupError(fmt.Errorf("iouring_setup: %w", syscall.EPERM))) {
		t.Error("EPERM (seccomp, kernel.io_uring_disabled) should select the epoll fallback")
	}
	if ioURingUnavailable(setupError(syscall.ENOMEM)) {
		t.Error("ENOMEM from setup is a configuration problem, not a reason to fall back")
	}
	if !ioURingUnavailable(fmt.Errorf("%w: running 5.15", ErrKernelTooOld)) {
		t.Error("an old kernel should select the epoll fallback")
	}
	if ioURingUnavailable(syscall.ENOMEM) {
		t.Error("ENOMEM is a configuration problem, not a reason to fall back")
	}
	s, _ := serve(t, &apiEcho{}, WithRings(1), WithBackend(BackendIOUring))
	if defaultBackend != BackendEpoll && s.Backend() != BackendIOUring {
		t.Fatalf("Backend() = %v", s.Backend())
	}
}

// The epoll backend must also be correct with very few buffers: pause and resume after -ENOBUFS.
func TestEpollBufferStarvation(t *testing.T) {
	_, addr := serve(t, &splitEcho{}, WithRings(1), WithBuffers(4, 512), WithBackend(BackendEpoll))
	data := bytes.Repeat([]byte("0123456789"), 50000)
	if err := echoChunked(addr, data, 3); err != nil {
		t.Fatal(err)
	}
}
