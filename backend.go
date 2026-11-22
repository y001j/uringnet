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

// defaultBackend 是没有指定 Backend 时使用的后端；测试用环境变量 URINGNET_BACKEND 改变它
var defaultBackend = BackendAuto

// handler 是异步操作的完成回调：res 与 flags 的含义同 io_uring 的 CQE
type handler = iouring.Handler

// eventLoop 是事件循环后端。所有提交方法只能在循环线程中调用，完成回调也在循环线程中执行，
// 并且总是在提交方法返回之后才执行（与 io_uring 的语义一致）。
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

// bufRing 是接收缓冲区池（provided buffer ring 或其模拟）
type bufRing interface {
	Get(res int32, flags uint32) (bid uint16, data []byte, ok bool)
	Bundle(res int32, flags uint32, dst []iouring.BundleBuf) []iouring.BundleBuf
	GetIncremental(res int32, flags uint32) (bid uint16, data []byte, more, ok bool)
	Recycle(bid uint16)
	FixedIndex() int
}

// ---- io_uring 后端 ----

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

// kernelTimespec 与内核的 struct __kernel_timespec 布局一致（在 32 位平台上也是两个 int64）
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
		// len = 1（一个 timespec），off = 0：只在到期时完成，不按完成事件数计数
		sqe.PrepOperation(iouring_syscall.IORING_OP_TIMEOUT, -1, uint64(uintptr(unsafe.Pointer(&buf[0]))), 1, 0)
	}, buf, h)
}

func (u *uringLoop) Connect(fd int, raw []byte, h handler) uint64 {
	return u.l.Prep(func(sqe *iouring_syscall.SubmissionQueueEntry) {
		// connect：addr 为 sockaddr，off 为其长度
		sqe.PrepOperation(iouring_syscall.IORING_OP_CONNECT, int32(fd), uint64(uintptr(unsafe.Pointer(&raw[0]))), 0, uint64(len(raw)))
	}, raw, h)
}

func (u *uringLoop) RecvMsgMultishot(fd int, msg *unix.Msghdr, h handler) uint64 {
	return u.l.Prep(func(sqe *iouring_syscall.SubmissionQueueEntry) {
		sqe.PrepOperation(iouring_syscall.IORING_OP_RECVMSG, int32(fd), uint64(uintptr(unsafe.Pointer(msg))), 1, 0)
		sqe.SetIoprio(iouring_syscall.IORING_RECV_MULTISHOT)
		sqe.SetFlags(iouring_syscall.IOSQE_FLAGS_BUFFER_SELECT)
		sqe.SetBufGroup(0) // Loop 按下标分配组号，这里只有第 0 个 buffer ring
	}, nil, h)
}

// ioURingUnavailable 判断创建 io_uring 失败是否意味着应该换用 epoll
func ioURingUnavailable(err error) bool {
	return unavailable(err) || stderrors.Is(err, ErrIOUringUnavailable) || stderrors.Is(err, ErrKernelTooOld)
}
