//go:build linux
// +build linux

package uringnet

import (
	stderrors "errors"
	"fmt"
	"time"
	"unsafe"

	iouring "github.com/y001j/iouring-go"
	iouring_syscall "github.com/y001j/iouring-go/syscall"
	"golang.org/x/sys/unix"
)

// Backend selects the I/O mechanism of the event loops.
type Backend int

const (
	// BackendAuto uses io_uring and falls back to epoll when io_uring is unavailable
	// (kernel older than 6.0, blocked by seccomp, or kernel.io_uring_disabled). The fallback
	// is logged as a warning.
	BackendAuto Backend = iota
	// BackendIOUring uses io_uring only; creating the server fails if it is unavailable.
	BackendIOUring
	// BackendEpoll uses epoll. It supports everything except zero-copy send, recv bundles
	// and incremental buffers, which are silently disabled.
	BackendEpoll
)

func (b Backend) String() string {
	switch b {
	case BackendAuto:
		return "auto"
	case BackendIOUring:
		return "io_uring"
	case BackendEpoll:
		return "epoll"
	}
	return fmt.Sprintf("Backend(%d)", int(b))
}

// defaultBackend is used when no Backend is chosen; tests change it with URINGNET_BACKEND.
var defaultBackend = BackendAuto

// handler is the completion callback of an asynchronous operation; res and flags mean what they mean in an io_uring CQE.
type handler = iouring.Handler

// eventLoop is an event loop backend. Submissions must happen on the loop thread; completions run on the loop
// thread too, always after the submitting call has returned (the io_uring semantics).
type eventLoop interface {
	Post(fn func()) error
	Close() error
	Done() <-chan struct{}
	Features() uint32
	BufferRing() bufRing

	AcceptMultishot(fd int, h handler) uint64
	RecvMultishot(fd int, bundle bool, h handler) uint64
	Send(fd int, b []byte, h handler) uint64
	Sendmsg(fd int, msg *unix.Msghdr, keep []byte, h handler) uint64
	SendZC(fd int, b []byte, fixedIdx int, h handler) uint64
	Cancel(ud uint64)
	Timeout(d time.Duration, h handler)
	Connect(fd int, raw []byte, h handler) uint64
	RecvMsgMultishot(fd int, msg *unix.Msghdr, h handler) uint64
}

// bufRing is the receive buffer pool (a provided buffer ring, or its emulation).
type bufRing interface {
	Get(res int32, flags uint32) (bid uint16, data []byte, ok bool)
	Bundle(res int32, flags uint32, dst []iouring.BundleBuf) []iouring.BundleBuf
	GetIncremental(res int32, flags uint32) (bid uint16, data []byte, more, ok bool)
	Recycle(bid uint16)
	FixedIndex() int
}

// ---- io_uring backend ----

type uringLoop struct {
	l  *iouring.Loop
	br *iouring.BufferRing
}

func newURingLoop(cfg iouring.LoopConfig) (*uringLoop, error) {
	l, err := iouring.NewLoop(cfg)
	if err != nil {
		return nil, err
	}
	return &uringLoop{l: l, br: l.BufferRing(0)}, nil
}

func (u *uringLoop) Post(fn func()) error  { return u.l.Post(fn) }
func (u *uringLoop) Close() error          { return u.l.Close() }
func (u *uringLoop) Done() <-chan struct{} { return u.l.Done() }
func (u *uringLoop) Features() uint32      { return u.l.Features() }
func (u *uringLoop) BufferRing() bufRing   { return u.br }

func (u *uringLoop) AcceptMultishot(fd int, h handler) uint64 {
	return u.l.AcceptMultishot(fd, unix.SOCK_CLOEXEC, h)
}

func (u *uringLoop) RecvMultishot(fd int, bundle bool, h handler) uint64 {
	if bundle {
		return u.l.RecvMultishotBundle(fd, u.br, 0, h)
	}
	return u.l.RecvMultishot(fd, u.br, 0, h)
}

func (u *uringLoop) Send(fd int, b []byte, h handler) uint64 {
	return u.l.Send(fd, b, unix.MSG_NOSIGNAL, h)
}

func (u *uringLoop) Sendmsg(fd int, msg *unix.Msghdr, keep []byte, h handler) uint64 {
	if keep == nil {
		return u.l.Sendmsg(fd, msg, unix.MSG_NOSIGNAL, h)
	}
	return u.l.Prep(func(sqe *iouring_syscall.SubmissionQueueEntry) {
		sqe.PrepOperation(iouring_syscall.IORING_OP_SENDMSG, int32(fd), uint64(uintptr(unsafe.Pointer(msg))), 1, 0)
		sqe.SetOpFlags(unix.MSG_NOSIGNAL)
	}, keep, h)
}

func (u *uringLoop) SendZC(fd int, b []byte, fixedIdx int, h handler) uint64 {
	if fixedIdx >= 0 {
		return u.l.SendZCFixed(fd, b, uint16(fixedIdx), unix.MSG_NOSIGNAL, 0, h)
	}
	return u.l.SendZC(fd, b, unix.MSG_NOSIGNAL, 0, h)
}

func (u *uringLoop) Cancel(ud uint64) { u.l.Cancel(ud, func(int32, uint32) {}) }

// kernelTimespec matches the kernel's struct __kernel_timespec (two int64 also on 32-bit platforms).
type kernelTimespec struct {
	sec  int64
	nsec int64
}

func (u *uringLoop) Timeout(d time.Duration, h handler) {
	if d <= 0 {
		d = time.Nanosecond
	}
	buf := make([]byte, unsafe.Sizeof(kernelTimespec{}))
	ts := (*kernelTimespec)(unsafe.Pointer(&buf[0]))
	ts.sec = int64(d / time.Second)
	ts.nsec = int64(d % time.Second)
	u.l.Prep(func(sqe *iouring_syscall.SubmissionQueueEntry) {
		// len = 1 (one timespec), off = 0: complete on expiry only, not after a number of completions
		sqe.PrepOperation(iouring_syscall.IORING_OP_TIMEOUT, -1, uint64(uintptr(unsafe.Pointer(&buf[0]))), 1, 0)
	}, buf, h)
}

func (u *uringLoop) Connect(fd int, raw []byte, h handler) uint64 {
	return u.l.Prep(func(sqe *iouring_syscall.SubmissionQueueEntry) {
		// connect: addr is the sockaddr, off its length
		sqe.PrepOperation(iouring_syscall.IORING_OP_CONNECT, int32(fd), uint64(uintptr(unsafe.Pointer(&raw[0]))), 0, uint64(len(raw)))
	}, raw, h)
}

func (u *uringLoop) RecvMsgMultishot(fd int, msg *unix.Msghdr, h handler) uint64 {
	return u.l.Prep(func(sqe *iouring_syscall.SubmissionQueueEntry) {
		sqe.PrepOperation(iouring_syscall.IORING_OP_RECVMSG, int32(fd), uint64(uintptr(unsafe.Pointer(msg))), 1, 0)
		sqe.SetIoprio(iouring_syscall.IORING_RECV_MULTISHOT)
		sqe.SetFlags(iouring_syscall.IOSQE_FLAGS_BUFFER_SELECT)
		sqe.SetBufGroup(0) // the Loop numbers buffer rings by index; there is only ring 0
	}, nil, h)
}

// ioURingUnavailable reports whether failing to create an io_uring means epoll should be used instead.
func ioURingUnavailable(err error) bool {
	return unavailable(err) || stderrors.Is(err, ErrIOUringUnavailable) || stderrors.Is(err, ErrKernelTooOld)
}
