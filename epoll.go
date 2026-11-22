//go:build linux
// +build linux

package uringnet

import (
	"container/heap"
	"encoding/binary"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	iouring "github.com/y001j/iouring-go"
	iouring_syscall "github.com/y001j/iouring-go/syscall"
	"golang.org/x/sys/unix"
)

// epollLoop is the fallback backend. It emulates the io_uring operations the rest of the
// package uses with non-blocking sockets and level-triggered epoll: multishot accept and
// recv keep reading while the fd is readable, receives take buffers from an emulated
// provided-buffer pool (-ENOBUFS when it is empty), sends retry on EPOLLOUT, and every
// completion is queued and delivered after the submitting call returns, as io_uring does.
type epollLoop struct {
	epfd, evfd int
	br         *epollBufRing

	mu       sync.Mutex
	posted   []func()
	spare    []func()
	stopping atomic.Bool
	done     chan struct{}

	// 以下字段只在循环线程中访问
	nextUD uint64
	fds    map[int]*epFD
	ops    map[uint64]*epOp // 可取消的操作：accept、recv、connect
	ready  []completion
	timers timerHeap
}

type completion struct {
	h     handler
	res   int32
	flags uint32
}

const (
	opAccept = iota
	opRecv
	opRecvMsg
	opConnect
)

type epOp struct {
	ud   uint64
	fd   int
	kind int
	h    handler
}

// epFD 是一个 fd 上进行中的操作
type epFD struct {
	fd         int
	registered uint32 // 当前注册的 epoll 事件，0 表示未注册
	accept     *epOp
	recv       *epOp // 流式 recv 或 recvmsg
	nameLen    uint32
	connect    *epOp
	sends      []epSend // 因 EAGAIN 等待 EPOLLOUT 的发送，按顺序
}

type epSend struct {
	b    []byte
	msg  *unix.Msghdr
	keep []byte
	h    handler
}

func newEpollLoop(count, size int) (*epollLoop, error) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	evfd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		unix.Close(epfd)
		return nil, err
	}
	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, evfd, &unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(evfd)}); err != nil {
		unix.Close(epfd)
		unix.Close(evfd)
		return nil, err
	}
	l := &epollLoop{epfd: epfd, evfd: evfd, br: newEpollBufRing(count, size), done: make(chan struct{}),
		fds: make(map[int]*epFD), ops: make(map[uint64]*epOp)}
	go l.run()
	return l, nil
}

func (l *epollLoop) Post(fn func()) error {
	if l.stopping.Load() {
		return ErrServerClosed
	}
	l.mu.Lock()
	l.posted = append(l.posted, fn)
	l.mu.Unlock()
	l.wake()
	return nil
}

func (l *epollLoop) wake() {
	var one = [8]byte{1}
	unix.Write(l.evfd, one[:])
}

func (l *epollLoop) Close() error {
	if l.stopping.CompareAndSwap(false, true) {
		l.wake()
	}
	<-l.done
	return nil
}

func (l *epollLoop) Done() <-chan struct{} { return l.done }
func (l *epollLoop) Features() uint32      { return 0 }
func (l *epollLoop) BufferRing() bufRing   { return l.br }

func (l *epollLoop) run() {
	runtime.LockOSThread()
	defer func() {
		unix.Close(l.epfd)
		unix.Close(l.evfd)
		close(l.done)
	}()
	events := make([]unix.EpollEvent, 256)
	for {
		l.runPosted()
		l.deliver()
		if l.stopping.Load() {
			return
		}
		timeout := -1
		if len(l.ready) > 0 {
			timeout = 0
		} else if len(l.timers) > 0 {
			d := time.Until(l.timers[0].when)
			if d <= 0 {
				timeout = 0
			} else {
				timeout = int((d + time.Millisecond - 1) / time.Millisecond)
			}
		}
		n, err := unix.EpollWait(l.epfd, events, timeout)
		if err != nil && err != unix.EINTR {
			return
		}
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			if fd == l.evfd {
				var buf [8]byte
				unix.Read(l.evfd, buf[:])
				continue
			}
			if st := l.fds[fd]; st != nil {
				l.handle(st, events[i].Events)
			}
		}
		l.fireTimers()
	}
}

func (l *epollLoop) runPosted() {
	l.mu.Lock()
	fns := l.posted
	l.posted = l.spare[:0]
	l.mu.Unlock()
	for i, fn := range fns {
		fn()
		fns[i] = nil
	}
	l.spare = fns
}

// deliver 执行排队的完成回调；回调中提交的操作产生的新完成也在这里一并执行
func (l *epollLoop) deliver() {
	for len(l.ready) > 0 {
		batch := l.ready
		l.ready = nil
		for _, c := range batch {
			c.h(c.res, c.flags)
		}
	}
}

func (l *epollLoop) complete(h handler, res int32, flags uint32) {
	l.ready = append(l.ready, completion{h, res, flags})
}

func errnoRes(err error) int32 {
	if e, ok := err.(syscall.Errno); ok {
		return -int32(e)
	}
	return -int32(syscall.EIO)
}

func (l *epollLoop) newOp(fd, kind int, h handler) *epOp {
	l.nextUD++
	op := &epOp{ud: l.nextUD, fd: fd, kind: kind, h: h}
	l.ops[op.ud] = op
	return op
}

func (l *epollLoop) endOp(op *epOp) { delete(l.ops, op.ud) }

// fdState 返回 fd 的状态，第一次使用时把 fd 设为非阻塞
func (l *epollLoop) fdState(fd int) *epFD {
	st := l.fds[fd]
	if st == nil {
		unix.SetNonblock(fd, true)
		st = &epFD{fd: fd}
		l.fds[fd] = st
	}
	return st
}

// update 按 fd 上进行中的操作调整 epoll 注册；没有操作时注销并忘掉这个 fd
func (l *epollLoop) update(st *epFD) {
	var want uint32
	if st.accept != nil || st.recv != nil {
		want |= unix.EPOLLIN
	}
	if st.connect != nil || len(st.sends) > 0 {
		want |= unix.EPOLLOUT
	}
	switch {
	case want == st.registered:
	case want == 0:
		unix.EpollCtl(l.epfd, unix.EPOLL_CTL_DEL, st.fd, nil)
	case st.registered == 0:
		ev := unix.EpollEvent{Events: want, Fd: int32(st.fd)}
		if unix.EpollCtl(l.epfd, unix.EPOLL_CTL_ADD, st.fd, &ev) == unix.EEXIST {
			unix.EpollCtl(l.epfd, unix.EPOLL_CTL_MOD, st.fd, &ev)
		}
	default:
		unix.EpollCtl(l.epfd, unix.EPOLL_CTL_MOD, st.fd, &unix.EpollEvent{Events: want, Fd: int32(st.fd)})
	}
	st.registered = want
	if want == 0 {
		delete(l.fds, st.fd)
	}
}

func (l *epollLoop) handle(st *epFD, ev uint32) {
	if ev&(unix.EPOLLIN|unix.EPOLLERR|unix.EPOLLHUP) != 0 {
		switch {
		case st.accept != nil:
			l.doAccept(st)
		case st.recv != nil && st.recv.kind == opRecvMsg:
			l.doRecvMsg(st)
		case st.recv != nil:
			l.doRecv(st)
		}
	}
	if ev&(unix.EPOLLOUT|unix.EPOLLERR|unix.EPOLLHUP) != 0 {
		if st.connect != nil {
			l.finishConnect(st)
		}
		if len(st.sends) > 0 {
			l.flushSends(st)
		}
	}
	l.update(st)
}

// ---- accept ----

func (l *epollLoop) AcceptMultishot(fd int, h handler) uint64 {
	st := l.fdState(fd)
	st.accept = l.newOp(fd, opAccept, h)
	l.update(st)
	return st.accept.ud
}

func (l *epollLoop) doAccept(st *epFD) {
	op := st.accept
	for i := 0; i < 64; i++ {
		nfd, _, err := unix.Accept4(st.fd, unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		switch err {
		case nil:
			l.complete(op.h, int32(nfd), iouring_syscall.IORING_CQE_F_MORE)
			continue
		case unix.EAGAIN, unix.EINTR, unix.ECONNABORTED:
			if err == unix.EAGAIN {
				return
			}
			continue
		}
		// 与 io_uring 一样，出错时 multishot accept 终止，由调用方决定何时重新提交
		st.accept = nil
		l.endOp(op)
		l.complete(op.h, errnoRes(err), 0)
		return
	}
}

// ---- recv ----

func (l *epollLoop) RecvMultishot(fd int, _ bool, h handler) uint64 {
	st := l.fdState(fd)
	st.recv = l.newOp(fd, opRecv, h)
	l.update(st)
	return st.recv.ud
}

// endRecv 终止 multishot recv，以 res 完成（不带 MORE）
func (l *epollLoop) endRecv(st *epFD, res int32) {
	op := st.recv
	st.recv = nil
	l.endOp(op)
	l.complete(op.h, res, 0)
}

func bufFlags(bid uint16) uint32 {
	return iouring_syscall.IORING_CQE_F_BUFFER | uint32(bid)<<iouring_syscall.IORING_CQE_BUFFER_SHIFT | iouring_syscall.IORING_CQE_F_MORE
}

func (l *epollLoop) doRecv(st *epFD) {
	op := st.recv
	for i := 0; i < 16 && st.recv == op; i++ {
		bid, buf, ok := l.br.take()
		if !ok {
			l.endRecv(st, -int32(syscall.ENOBUFS))
			return
		}
		n, err := unix.Read(st.fd, buf)
		switch {
		case err == unix.EAGAIN:
			l.br.Recycle(bid)
			return
		case err == unix.EINTR:
			l.br.Recycle(bid)
		case err != nil:
			l.br.Recycle(bid)
			l.endRecv(st, errnoRes(err))
			return
		case n == 0:
			l.br.Recycle(bid)
			l.endRecv(st, 0)
			return
		default:
			l.complete(op.h, int32(n), bufFlags(bid))
			if n < len(buf) {
				// 没读满：socket 多半已经读空。不再试读（那只会多一次返回 EAGAIN 的系统调用），
				// 水平触发的 epoll 会在还有数据时再次通知
				return
			}
		}
	}
}

func (l *epollLoop) RecvMsgMultishot(fd int, msg *unix.Msghdr, h handler) uint64 {
	st := l.fdState(fd)
	st.recv = l.newOp(fd, opRecvMsg, h)
	st.nameLen = msg.Namelen
	l.update(st)
	return st.recv.ud
}

// doRecvMsg 收数据报，并按 io_uring multishot recvmsg 的格式写入缓冲区：
// io_uring_recvmsg_out（16 字节）、地址（nameLen 字节）、数据
func (l *epollLoop) doRecvMsg(st *epFD) {
	op := st.recv
	nameLen := st.nameLen
	off := recvmsgOutLen + int(nameLen)
	for i := 0; i < 16 && st.recv == op; i++ {
		bid, buf, ok := l.br.take()
		if !ok {
			l.endRecv(st, -int32(syscall.ENOBUFS))
			return
		}
		if len(buf) <= off {
			l.br.Recycle(bid)
			l.endRecv(st, -int32(syscall.ENOBUFS))
			return
		}
		var iov unix.Iovec
		iov.Base = &buf[off]
		iov.SetLen(len(buf) - off)
		msg := unix.Msghdr{Name: &buf[recvmsgOutLen], Namelen: nameLen, Iov: &iov}
		msg.SetIovlen(1)
		n, _, e := unix.Syscall(unix.SYS_RECVMSG, uintptr(st.fd), uintptr(unsafe.Pointer(&msg)), unix.MSG_TRUNC)
		switch {
		case e == unix.EAGAIN:
			l.br.Recycle(bid)
			return
		case e == unix.EINTR:
			l.br.Recycle(bid)
			continue
		case e != 0:
			l.br.Recycle(bid)
			l.endRecv(st, errnoRes(e))
			return
		}
		binary.NativeEndian.PutUint32(buf[0:], msg.Namelen)
		binary.NativeEndian.PutUint32(buf[4:], 0)
		binary.NativeEndian.PutUint32(buf[8:], uint32(n)) // MSG_TRUNC：数据报的实际长度
		binary.NativeEndian.PutUint32(buf[12:], uint32(msg.Flags))
		got := int(n)
		if got > len(buf)-off {
			got = len(buf) - off
		}
		l.complete(op.h, int32(off+got), bufFlags(bid))
	}
}

// ---- send ----

func (l *epollLoop) Send(fd int, b []byte, h handler) uint64 {
	return l.submitSend(fd, epSend{b: b, h: h})
}

func (l *epollLoop) Sendmsg(fd int, msg *unix.Msghdr, keep []byte, h handler) uint64 {
	return l.submitSend(fd, epSend{msg: msg, keep: keep, h: h})
}

// SendZC 在 epoll 下退化为普通发送（启用 epoll 时零拷贝已关闭，这里只是兜底）
func (l *epollLoop) SendZC(fd int, b []byte, _ int, h handler) uint64 {
	return l.Send(fd, b, h)
}

func (l *epollLoop) submitSend(fd int, s epSend) uint64 {
	l.nextUD++
	st := l.fdState(fd)
	if len(st.sends) == 0 {
		if res, again := trySend(fd, s); !again {
			l.complete(s.h, res, 0)
			if st.accept == nil && st.recv == nil && st.connect == nil {
				l.update(st) // 没有其他操作时忘掉这个 fd
			}
			return l.nextUD
		}
	}
	st.sends = append(st.sends, s)
	l.update(st)
	return l.nextUD
}

// trySend 发送一次；again 为 true 表示 socket 缓冲区已满，需要等 EPOLLOUT
func trySend(fd int, s epSend) (res int32, again bool) {
	for {
		var n uintptr
		var e syscall.Errno
		if s.msg != nil {
			n, _, e = unix.Syscall(unix.SYS_SENDMSG, uintptr(fd), uintptr(unsafe.Pointer(s.msg)), unix.MSG_NOSIGNAL)
		} else {
			var p unsafe.Pointer
			if len(s.b) > 0 {
				p = unsafe.Pointer(&s.b[0])
			}
			n, _, e = unix.Syscall6(unix.SYS_SENDTO, uintptr(fd), uintptr(p), uintptr(len(s.b)), unix.MSG_NOSIGNAL, 0, 0)
		}
		switch e {
		case 0:
			return int32(n), false
		case unix.EINTR:
			continue
		case unix.EAGAIN:
			return 0, true
		}
		return -int32(e), false
	}
}

func (l *epollLoop) flushSends(st *epFD) {
	for len(st.sends) > 0 {
		s := st.sends[0]
		res, again := trySend(st.fd, s)
		if again {
			return
		}
		st.sends[0] = epSend{}
		st.sends = st.sends[1:]
		l.complete(s.h, res, 0)
	}
	st.sends = nil
}

// ---- connect ----

func (l *epollLoop) Connect(fd int, raw []byte, h handler) uint64 {
	st := l.fdState(fd)
	op := l.newOp(fd, opConnect, h)
	for {
		_, _, e := unix.Syscall(unix.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw)))
		switch e {
		case unix.EINTR:
			continue
		case unix.EINPROGRESS:
			st.connect = op
			l.update(st)
			return op.ud
		case 0:
			l.endOp(op)
			l.complete(h, 0, 0)
		default:
			l.endOp(op)
			l.complete(h, -int32(e), 0)
		}
		l.update(st)
		return op.ud
	}
}

func (l *epollLoop) finishConnect(st *epFD) {
	op := st.connect
	st.connect = nil
	l.endOp(op)
	v, err := unix.GetsockoptInt(st.fd, unix.SOL_SOCKET, unix.SO_ERROR)
	switch {
	case err != nil:
		l.complete(op.h, errnoRes(err), 0)
	case v != 0:
		l.complete(op.h, -int32(v), 0)
	default:
		l.complete(op.h, 0, 0)
	}
}

// ---- cancel ----

func (l *epollLoop) Cancel(ud uint64) {
	op := l.ops[ud]
	if op == nil {
		return // 已经完成
	}
	st := l.fds[op.fd]
	l.endOp(op)
	if st != nil {
		switch {
		case st.recv == op:
			st.recv = nil
		case st.accept == op:
			st.accept = nil
		case st.connect == op:
			st.connect = nil
		}
		l.update(st)
	}
	l.complete(op.h, -int32(syscall.ECANCELED), 0)
}

// ---- timers ----

type epTimer struct {
	when time.Time
	h    handler
}

type timerHeap []epTimer

func (t timerHeap) Len() int            { return len(t) }
func (t timerHeap) Less(i, j int) bool  { return t[i].when.Before(t[j].when) }
func (t timerHeap) Swap(i, j int)       { t[i], t[j] = t[j], t[i] }
func (t *timerHeap) Push(x interface{}) { *t = append(*t, x.(epTimer)) }
func (t *timerHeap) Pop() interface{} {
	old := *t
	x := old[len(old)-1]
	*t = old[:len(old)-1]
	return x
}

func (l *epollLoop) Timeout(d time.Duration, h handler) {
	heap.Push(&l.timers, epTimer{when: time.Now().Add(d), h: h})
}

func (l *epollLoop) fireTimers() {
	if len(l.timers) == 0 {
		return
	}
	now := time.Now()
	for len(l.timers) > 0 && !l.timers[0].when.After(now) {
		t := heap.Pop(&l.timers).(epTimer)
		l.complete(t.h, -int32(syscall.ETIME), 0)
	}
}

// ---- buffer pool ----

// epollBufRing 模拟 provided buffer ring：空闲缓冲区是一个栈（后进先出），
// 最近归还的缓冲区最先被复用，拷贝时更可能还在 CPU 缓存里
type epollBufRing struct {
	size int
	mem  []byte
	free []uint16
}

func newEpollBufRing(count, size int) *epollBufRing {
	b := &epollBufRing{size: size, mem: make([]byte, count*size), free: make([]uint16, 0, count)}
	for i := count - 1; i >= 0; i-- {
		b.free = append(b.free, uint16(i))
	}
	return b
}

func (b *epollBufRing) buf(bid uint16) []byte {
	off := int(bid) * b.size
	return b.mem[off : off+b.size : off+b.size]
}

func (b *epollBufRing) take() (uint16, []byte, bool) {
	n := len(b.free)
	if n == 0 {
		return 0, nil, false
	}
	bid := b.free[n-1]
	b.free = b.free[:n-1]
	return bid, b.buf(bid), true
}

func (b *epollBufRing) Get(res int32, flags uint32) (uint16, []byte, bool) {
	if flags&iouring_syscall.IORING_CQE_F_BUFFER == 0 {
		return 0, nil, false
	}
	bid := uint16(flags >> iouring_syscall.IORING_CQE_BUFFER_SHIFT)
	n := int(res)
	if n < 0 {
		n = 0
	}
	return bid, b.buf(bid)[:n], true
}

func (b *epollBufRing) Recycle(bid uint16) { b.free = append(b.free, bid) }

func (b *epollBufRing) Bundle(int32, uint32, []iouring.BundleBuf) []iouring.BundleBuf { return nil }

func (b *epollBufRing) GetIncremental(int32, uint32) (uint16, []byte, bool, bool) {
	return 0, nil, false, false
}

func (b *epollBufRing) FixedIndex() int { return -1 }
