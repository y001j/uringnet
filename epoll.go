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

	// The fields below are only accessed on the loop thread.
	nextUD uint64
	fds    map[int]*epFD
	ops    map[uint64]*epOp // cancellable operations: accept, recv, connect
	ready  []completion
	timers timerHeap
	iov    [2]unix.Iovec // readv vector of doRecv, reused
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

// epFD holds the operations pending on one fd.
type epFD struct {
	fd         int
	registered uint32 // epoll events currently registered, 0 if none
	accept     *epOp
	recv       *epOp // stream recv or recvmsg
	nameLen    uint32
	connect    *epOp
	sends      []epSend // sends waiting for EPOLLOUT after EAGAIN, in order
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
				// Run this fd's completions now rather than after the whole batch: otherwise the first
				// connection's reply waits until every fd in the batch has been read, which adds latency
				// and, with request/response traffic, costs throughput
				l.deliver()
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

// deliver runs the queued completions, including those produced by operations the completions submit.
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

// fdState returns the state of fd, making the fd non-blocking on first use.
func (l *epollLoop) fdState(fd int) *epFD {
	st := l.fds[fd]
	if st == nil {
		unix.SetNonblock(fd, true)
		st = &epFD{fd: fd}
		l.fds[fd] = st
	}
	return st
}

// update adjusts the epoll registration to the pending operations; with none it unregisters and forgets the fd.
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
		// As with io_uring, an error ends the multishot accept and the caller decides when to re-arm
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

// endRecv ends the multishot recv, completing with res (without MORE).
func (l *epollLoop) endRecv(st *epFD, res int32) {
	op := st.recv
	st.recv = nil
	l.endOp(op)
	l.complete(op.h, res, 0)
}

func bufFlags(bid uint16) uint32 {
	return iouring_syscall.IORING_CQE_F_BUFFER | uint32(bid)<<iouring_syscall.IORING_CQE_BUFFER_SHIFT | iouring_syscall.IORING_CQE_F_MORE
}

// doRecv reads with readv into two buffers at a time. If the second one stays empty the socket is
// drained, which a plain read can only tell by trying again and getting EAGAIN (an extra syscall
// for every message that fills a buffer exactly); if both fill up, more data is likely waiting and
// it reads again. Level-triggered epoll reports the fd again if anything is left after the limit.
func (l *epollLoop) doRecv(st *epFD) {
	op := st.recv
	for i := 0; i < 8 && st.recv == op; i++ {
		bid, buf, ok := l.br.take()
		if !ok {
			l.endRecv(st, -int32(syscall.ENOBUFS))
			return
		}
		bid2, buf2, ok2 := l.br.take()
		var n int
		var err error
		if ok2 {
			l.iov[0].Base, l.iov[1].Base = &buf[0], &buf2[0]
			l.iov[0].SetLen(len(buf))
			l.iov[1].SetLen(len(buf2))
			r, _, e := unix.Syscall(unix.SYS_READV, uintptr(st.fd), uintptr(unsafe.Pointer(&l.iov[0])), 2)
			n = int(r)
			if e != 0 {
				n, err = 0, e
			}
		} else {
			n, err = unix.Read(st.fd, buf)
		}
		if err != nil || n <= len(buf) {
			if ok2 {
				l.br.Recycle(bid2)
			}
		}
		switch {
		case err == unix.EAGAIN:
			l.br.Recycle(bid)
			return
		case err == unix.EINTR:
			l.br.Recycle(bid)
			continue
		case err != nil:
			l.br.Recycle(bid)
			l.endRecv(st, errnoRes(err))
			return
		case n == 0:
			l.br.Recycle(bid)
			l.endRecv(st, 0)
			return
		case n <= len(buf):
			l.complete(op.h, int32(n), bufFlags(bid))
			if n < len(buf) || ok2 {
				return // drained (a full first buffer with an empty second one also means drained)
			}
		default: // both buffers used
			l.complete(op.h, int32(len(buf)), bufFlags(bid))
			l.complete(op.h, int32(n-len(buf)), bufFlags(bid2))
			if n < len(buf)+len(buf2) {
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

// doRecvMsg receives datagrams and lays them out in the buffer like io_uring's multishot recvmsg:
// io_uring_recvmsg_out (16 bytes), the address (nameLen bytes), then the payload.
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
		binary.NativeEndian.PutUint32(buf[8:], uint32(n)) // with MSG_TRUNC: the datagram's real length
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

// SendZC is a normal send on epoll (zero copy is turned off with epoll; this is only a safety net).
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
				l.update(st) // forget the fd if nothing else is pending on it
			}
			return l.nextUD
		}
	}
	st.sends = append(st.sends, s)
	l.update(st)
	return l.nextUD
}

// trySend sends once; again means the socket buffer is full and the send must wait for EPOLLOUT.
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
		return // already completed
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

// epollBufRing emulates a provided buffer ring. Free buffers form a stack (last in, first out), so the
// most recently returned buffer is reused first and is more likely to still be in the CPU cache.
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
