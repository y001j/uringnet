//go:build linux
// +build linux

package uringnet

import (
	"io"
	"net"
	"syscall"
	"unsafe"

	iouring "github.com/y001j/iouring-go"
	iouring_syscall "github.com/y001j/iouring-go/syscall"
	"golang.org/x/sys/unix"
)

// seg is one piece of data in a send queue.
type seg struct {
	b       []byte
	bid     int32  // >= 0: b lies in this receive buffer, returned after the send
	release func() // non-nil: b belongs to the user (WriteNoCopy); called once the kernel is done with it
	copied  bool   // b is the package's own copy buffer (may be appended to, reused after the send)
	zc      *zcRef // non-nil: sent with SEND_ZC
}

// zcRef tracks the notifications of a zero-copy segment: it can be released once sent (done) and all notification CQEs arrived (n == 0).
type zcRef struct {
	n       int
	done    bool
	bid     int32
	release func()
}

// conn is the state machine of one stream connection, accessed only on its ring's loop thread.
//
//   - Receive: one multishot recv keeps receiving into the provided buffer ring; with recv bundles a
//     completion can carry several buffers, each passed to OnTraffic.
//   - Send: the queue is made of segments with at most one send in flight, which keeps the stream in
//     order; short writes are handled. Data written during a callback (all buffers of a bundle) is sent
//     together after it, up to maxIov segments per sendmsg; small copied writes are merged into one segment.
//   - In-place send: data that lies in the current receive buffer (echo) is sent from it when the queue is empty, and the buffer returned afterwards.
//   - Zero copy: with ZeroCopyThreshold, large in-place sends and WriteNoCopy data use SEND_ZC
//     (SendZCFixed when the buffers are registered); the memory is released after the notification CQE.
//   - Close: shutdown(fd) ends the pending recv with EOF and the pending send with an error; the fd is
//     closed only after both have ended, so a reused fd number cannot get mixed up with a new connection.
type conn struct {
	ud UserData
	rn *URingNet
	fd int

	recvActive bool
	recvUD     uint64 // user_data of the multishot recv (to cancel it when pausing)
	paused     bool   // receiving paused: send queue above MaxWriteBuffer
	qBytes     int    // bytes queued and not yet sent
	lastActive uint64 // tick of the last data received or sent (IdleTimeout)
	lastSend   uint64 // tick of the last send progress, or when the queue became non-empty (WriteTimeout)
	sending    bool
	q          []seg
	qBase      []seg // start of q's backing array, reused once the queue empties to avoid allocating per enqueue
	off        int   // bytes of the head segment already sent
	inflight   int   // head segments covered by the send in flight
	iov        [maxIov]unix.Iovec
	msg        unix.Msghdr
	spare      [][]byte // reusable copy buffers
	closeAfter bool     // close once the queue is sent (EchoAndClose, Conn.Close)
	batching   bool     // inside a callback: queued data is sent by flush after it returns
	closed     bool     // shut down, waiting for pending requests to end
	freed      bool
	err        error // reason for closing, passed to OnClose

	// Receive buffer of the current OnTraffic call: data written from it can be sent in place (curBid < 0: not in OnTraffic)
	curBuf  []byte
	curBid  int32
	curHeld bool // the current receive buffer is held by the send queue and must not be returned after the callback

	api    Conn      // the connection object passed to Handler callbacks
	opened bool      // OnOpen has been called (OnClose is called only then)
	tls    *tlsState // non-nil for TLS connections

	onRecvH iouring.Handler
	onSendH iouring.Handler
}

// maxIov is the maximum number of segments in one gather send.
const maxIov = 16

func newConn(rn *URingNet, fd int) *conn {
	c := &conn{rn: rn, fd: fd, lastActive: rn.tick, lastSend: rn.tick, curBid: -1}
	c.ud.Fd = int32(fd)
	c.ud.c = c
	c.api.c = c
	// Cache the method values so submitting a request does not allocate a closure
	c.onRecvH = c.onRecv
	c.onSendH = c.onSend
	return c
}

func (c *conn) armRecv() {
	c.recvActive = true
	c.recvUD = c.rn.loop.RecvMultishot(c.fd, c.rn.bundle, c.onRecvH)
}

// checkBackpressure runs after a callback: above the queue limit, receiving pauses (the multishot recv is cancelled).
// Data received before the cancellation completes is still passed to OnTraffic.
func (c *conn) checkBackpressure() {
	max := c.rn.maxWrite
	if max <= 0 || c.paused || c.closed || c.qBytes <= max {
		return
	}
	c.paused = true
	c.rn.pauses++
	if c.recvActive {
		c.rn.loop.Cancel(c.recvUD)
	}
}

// maybeResume runs after send progress: receiving resumes once the queue is below half the limit.
func (c *conn) maybeResume() {
	if !c.paused || c.qBytes > c.rn.maxWrite/2 {
		return
	}
	c.paused = false
	if !c.recvActive && !c.closed {
		c.armRecv()
	}
}

func (c *conn) onRecv(res int32, flags uint32) {
	rn := c.rn
	if res > 0 {
		c.lastActive = rn.tick
		rn.bytesIn += uint64(res)
	}
	c.batching = true
	if rn.bundle {
		// One CQE may span several consecutive buffers; pass each to OnTraffic
		bufs := rn.br.Bundle(res, flags, rn.bundleBufs[:0])
		rn.bundleBufs = bufs[:0]
		for _, b := range bufs {
			c.deliver(b.Bid, b.Data)
		}
	} else if rn.inc {
		// Incremental: the data follows the previous data in a large buffer, which goes back once the kernel is done with it and no slice is referenced
		if bid, data, more, ok := rn.br.GetIncremental(res, flags); ok {
			if !more {
				rn.incDone[bid] = true
			}
			c.deliverInc(bid, data)
			rn.maybeRecycleInc(bid)
		}
	} else if bid, data, ok := rn.br.Get(res, flags); ok {
		c.deliver(bid, data)
	}
	c.batching = false
	c.flush()
	c.checkBackpressure()
	if flags&iouring_syscall.IORING_CQE_F_MORE != 0 {
		return
	}

	// The multishot recv has ended
	c.recvActive = false
	switch {
	case c.closed:
		c.maybeFree()
	case c.paused && (res > 0 || res == -int32(syscall.ECANCELED) || res == -int32(syscall.ENOBUFS)):
		// Paused by backpressure: maybeResume re-arms once the queue drains
	case res == -int32(syscall.ECANCELED):
		// Cancelled to pause, but the pause ended before the cancellation completed
		c.armRecv()
	case res == -int32(syscall.ENOBUFS):
		rn.enobufs++
		// Out of buffers. If some are held by sends in flight, re-arm once one comes back; otherwise all
		// processed buffers have already been returned and are published before the next submission, so retry now.
		if rn.held > 0 {
			rn.starved = append(rn.starved, c)
		} else {
			c.armRecv()
		}
	case res > 0:
		c.armRecv()
	case res == 0 && c.tls != nil:
		// TLS: deliver the records still buffered, then handle the EOF in tlsState.readDone (during
		// the handshake, the handshake goroutine sees the EOF and fails)
		c.tls.tr.end(io.EOF)
		if c.tls.ready {
			c.tls.pump()
		}
	case res == 0:
		// The peer shut down its write side (perhaps only half-closed, still waiting for a reply): stop receiving,
		// send what was written, then close; if the peer is fully gone the send fails and the connection closes
		if c.err == nil {
			c.err = io.EOF
		}
		c.closeAfterFlush()
	default:
		c.fail(syscall.Errno(-res))
	}
}

// deliver passes one receive buffer to OnTraffic and returns it at once unless the send queue holds it.
func (c *conn) deliver(bid uint16, data []byte) {
	held := c.traffic(bid, data)
	if !held {
		c.rn.br.Recycle(bid)
	}
}

// deliverInc is deliver for incremental buffers: in-place sends take references in writeFrom, and the caller
// returns the buffer once the kernel is done with it and nothing references it.
func (c *conn) deliverInc(bid uint16, data []byte) {
	c.traffic(bid, data)
}

// traffic calls OnTraffic and reports whether the receive buffer is now held by the send queue.
func (c *conn) traffic(bid uint16, data []byte) (held bool) {
	if len(data) == 0 || c.closed {
		return false
	}
	if c.tls != nil {
		if c.tls.ready {
			// crypto/tls copies everything available into its own record buffer, so the receive buffer
			// is only borrowed for the duration of pump and goes back at once
			c.tls.tr.lend(data)
			c.tls.pump()
			c.tls.tr.unlend()
		} else {
			c.tls.tr.feed(data) // copied for the handshake goroutine
		}
		return false
	}
	c.curBuf, c.curBid, c.curHeld = data, int32(bid), false
	c.rn.h.traffic(c, data)
	held = c.curHeld
	c.curBuf, c.curBid, c.curHeld = nil, -1, false
	return held
}

// send queues b, in place when it lies in the current receive buffer (during OnTraffic), otherwise copied.
func (c *conn) send(b []byte) {
	if c.tls != nil {
		c.tls.write(b)
		return
	}
	if c.writeFrom(b, c.curBuf, c.curBid) {
		c.curHeld = true
	}
}

// closeGracefully closes the connection once the send queue is empty, after a TLS close_notify.
func (c *conn) closeGracefully() {
	if c.tls != nil && !c.closeAfter && !c.closed {
		c.tls.closeNotify()
	}
	c.closeAfterFlush()
}

// closeAfterFlush closes the connection once the send queue is empty, at once if it already is.
func (c *conn) closeAfterFlush() {
	c.closeAfter = true
	if !c.sending && len(c.q) == 0 {
		c.close()
	}
}

// writeFrom queues b: in place (returns true) when the queue is empty and b lies in the current receive buffer,
// otherwise copied. Only with an empty queue: letting one connection hold several receive buffers makes running
// out of buffers (and re-arming recv after -ENOBUFS) more likely, which measured as a net loss.
// An in-place send holds the receive buffer until it completes; above maxHeld held buffers it copies instead,
// because running out ends the multishot recv with -ENOBUFS and re-arming costs far more than one copy.
func (c *conn) writeFrom(b, recvBuf []byte, bid int32) bool {
	if len(b) == 0 || c.closed {
		return false
	}
	if bid >= 0 && c.canHold(uint16(bid)) && within(b, recvBuf) {
		rn := c.rn
		rn.held++
		if rn.inc {
			// Each slice sent in place takes a reference on its buffer, released one by one in releaseBuf
			if rn.incRefs[bid] == 0 {
				rn.incPinned++
			}
			rn.incRefs[bid]++
		}
		c.enqueue(seg{b: b, bid: bid, release: nil})
		return true
	}
	c.write(b)
	return false
}

// canHold reports whether data in buffer bid can be sent in place.
// Normal mode: only with an empty send queue and fewer than maxHeld buffers held.
// Incremental mode: data takes only part of a large buffer, so a connection may send several slices in place;
// the limit is on referenced buffers (another slice of an already referenced buffer costs nothing).
func (c *conn) canHold(bid uint16) bool {
	rn := c.rn
	if rn.inc {
		return rn.incRefs[bid] > 0 || rn.incPinned < rn.maxHeld
	}
	return len(c.q) == 0 && rn.held < rn.maxHeld
}

// write copies b into the send queue, appending to the last copied segment when possible (not to segments covered by the send in flight).
func (c *conn) write(b []byte) {
	if len(b) == 0 || c.closed {
		return
	}
	if n := len(c.q); n > 0 {
		last := &c.q[n-1]
		if last.copied && n-1 >= c.inflight {
			last.b = append(last.b, b...)
			c.qBytes += len(b)
			return
		}
	}
	var buf []byte
	if n := len(c.spare); n > 0 {
		buf = c.spare[n-1][:0]
		c.spare = c.spare[:n-1]
	}
	c.enqueue(seg{b: append(buf, b...), bid: -1, copied: true})
}

// writeOwned queues b, which the package owns (no copy): appended to the last copied segment when
// possible, otherwise queued as a segment of its own that is reused as a copy buffer afterwards.
func (c *conn) writeOwned(b []byte) {
	if len(b) == 0 || c.closed {
		return
	}
	if n := len(c.q); n > 0 {
		if last := &c.q[n-1]; last.copied && n-1 >= c.inflight {
			last.b = append(last.b, b...)
			c.qBytes += len(b)
			return
		}
	}
	c.enqueue(seg{b: b, bid: -1, copied: true})
}

// writeNoCopy queues the user's b as is; release is called once the kernel is done with it.
func (c *conn) writeNoCopy(b []byte, release func()) error {
	if c.closed {
		if release != nil {
			release()
		}
		return errConnClosed
	}
	if len(b) == 0 {
		if release != nil {
			release()
		}
		return nil
	}
	if c.tls != nil {
		c.tls.write(b) // encrypting copies b, so it is free again at once
		if release != nil {
			release()
		}
		return nil
	}
	c.enqueue(seg{b: b, bid: -1, release: release})
	return nil
}

func (c *conn) enqueue(s seg) {
	rn := c.rn
	if !s.copied && rn.zcEnabled() && len(s.b) >= rn.ZeroCopyThreshold {
		s.zc = &zcRef{bid: s.bid, release: s.release}
	}
	if len(c.q) == 0 && c.qBase != nil {
		c.q = c.qBase[:0]
	}
	if len(c.q) == 0 {
		c.lastSend = rn.tick // WriteTimeout counts from here
	}
	c.q = append(c.q, s)
	c.qBytes += len(s.b)
	if len(c.q) == 1 {
		c.qBase = c.q // starting from an empty queue, q begins at the start of its backing array
	}
	if !c.sending && !c.batching {
		c.off = 0
		c.sendHead()
	}
}

// flush sends the data queued during a callback.
func (c *conn) flush() {
	if !c.sending && len(c.q) > 0 && !c.closed {
		c.off = 0
		c.sendHead()
	}
}

func (c *conn) sendHead() {
	c.sending = true
	s := &c.q[0]
	rn := c.rn
	if s.zc != nil && rn.zcEnabled() {
		s.zc.n++
		h := c.zcHandler(s.zc)
		fixed := -1
		if s.bid >= 0 {
			fixed = rn.fixedIdx
		}
		rn.loop.SendZC(c.fd, s.b[c.off:], fixed, h)
		rn.zcSends++
		c.inflight = 1
		return
	}
	// Gather send: consecutive non-zero-copy segments from the head, at most maxIov
	n := 0
	for i := range c.q {
		if n == maxIov || (i > 0 && c.q[i].zc != nil && rn.zcEnabled()) {
			break
		}
		b := c.q[i].b
		if i == 0 {
			b = b[c.off:]
		}
		c.iov[n].Base = &b[0]
		c.iov[n].SetLen(len(b))
		n++
	}
	c.inflight = n
	if n == 1 {
		rn.loop.Send(c.fd, s.b[c.off:], c.onSendH)
		return
	}
	c.msg = unix.Msghdr{Iov: &c.iov[0]}
	c.msg.SetIovlen(n)
	rn.loop.Sendmsg(c.fd, &c.msg, nil, c.onSendH)
	rn.gatherSends++
}

// zcHandler handles the result CQE and the notification CQE of a SEND_ZC.
func (c *conn) zcHandler(ref *zcRef) iouring.Handler {
	return func(res int32, flags uint32) {
		if flags&iouring_syscall.IORING_CQE_F_NOTIF != 0 {
			ref.n--
			c.rn.maybeReleaseZC(ref)
			return
		}
		if flags&iouring_syscall.IORING_CQE_F_MORE == 0 {
			ref.n-- // no notification CQE will follow
		}
		if res == -int32(syscall.EOPNOTSUPP) || res == -int32(syscall.EINVAL) || res == -int32(syscall.ENOMEM) {
			// The kernel or socket cannot do zero copy (EOPNOTSUPP/EINVAL), or pinning exceeds RLIMIT_MEMLOCK (ENOMEM):
			// that will not change soon, so turn zero copy off for this ring and resend normally
			c.rn.zcOff = true
			c.rn.log.Warn("uringnet: zero-copy send unavailable, falling back to normal sends", "ring", c.rn.index, "err", syscall.Errno(-res))
			c.sending = false
			if !c.closed {
				c.sendHead()
				return
			}
		}
		c.onSend(res, flags)
	}
}

func (c *conn) onSend(res int32, flags uint32) {
	c.inflight = 0
	if res <= 0 || c.closed {
		// Error (including -EPIPE / -ECANCELED after shutdown)
		c.sending = false
		if res < 0 {
			c.fail(syscall.Errno(-res))
		} else {
			c.fail(io.ErrShortWrite)
		}
		return
	}
	c.qBytes -= int(res)
	c.rn.bytesOut += uint64(res)
	c.lastActive, c.lastSend = c.rn.tick, c.rn.tick
	// Drop the segments covered by the bytes sent; a short write stops in the middle of a segment
	for remain := int(res); remain > 0; {
		left := len(c.q[0].b) - c.off
		if remain < left {
			c.off += remain
			break
		}
		remain -= left
		c.popHead()
		c.off = 0
	}
	c.maybeResume()
	if len(c.q) > 0 {
		c.sendHead()
		return
	}
	c.sending = false

	c.rn.h.written(c)
	if c.closeAfter {
		c.close()
	}
}

// popHead removes the fully sent head segment and releases its memory.
func (c *conn) popHead() {
	s := c.q[0]
	c.q[0] = seg{}
	c.q = c.q[1:]
	c.finish(s)
}

// finish releases a segment; zero-copy segments wait for all their notification CQEs.
func (c *conn) finish(s seg) {
	switch {
	case s.zc != nil:
		s.zc.done = true
		c.rn.maybeReleaseZC(s.zc)
	case s.copied:
		if len(c.spare) < 2 && cap(s.b) <= 1<<20 {
			c.spare = append(c.spare, s.b[:0])
		}
	default:
		c.rn.releaseBuf(s.bid, s.release)
	}
}

// fail closes the connection with err as the reason passed to OnClose; an earlier reason is kept.
func (c *conn) fail(err error) {
	if !c.closed && c.err == nil {
		c.err = err
	}
	c.close()
}

// close shuts the connection down so pending requests end soon; maybeFree closes the fd.
// It may be called repeatedly: the paths where pending requests end call it again to free the connection.
func (c *conn) close() {
	if !c.closed {
		c.closed = true
		unix.Shutdown(c.fd, unix.SHUT_RDWR)
	}
	c.maybeFree()
}

func (c *conn) maybeFree() {
	if c.recvActive || c.sending {
		return
	}
	c.free()
}

// free releases the connection. Normally called once no recv/send is pending; forced on Shutdown.
// Zero-copy memory is still released only after the notification CQEs (which do not depend on the fd).
func (c *conn) free() {
	if c.freed {
		return
	}
	c.freed = true
	c.closed = true
	delete(c.rn.conns, c.fd)
	c.rn.connCount.Add(-1)
	unix.Close(c.fd)
	if c.tls != nil {
		c.tls.tr.end(net.ErrClosed) // stops the TLS goroutine
	}
	if c.opened {
		c.rn.h.closed(c)
	}
	if !c.sending {
		for len(c.q) > 0 {
			c.popHead()
		}
	}
	c.spare = nil
}

// ---- receive buffer management (loop thread) ----

func (rn *URingNet) zcEnabled() bool { return rn.ZeroCopyThreshold > 0 && !rn.zcOff }

func (rn *URingNet) maybeReleaseZC(ref *zcRef) {
	if ref.done && ref.n == 0 {
		rn.releaseBuf(ref.bid, ref.release)
		ref.bid, ref.release = -1, nil
	}
}

// releaseBuf returns a receive buffer held by a send, or tells the user their buffer is free.
func (rn *URingNet) releaseBuf(bid int32, release func()) {
	if bid >= 0 {
		rn.held--
		if rn.inc {
			b := uint16(bid)
			if rn.incRefs[b]--; rn.incRefs[b] == 0 {
				rn.incPinned--
				rn.maybeRecycleInc(b)
			}
			// A buffer whose references dropped to zero may not be finished by the kernel yet (so it is not returned and wakes nobody);
			// when no slice is held at all the ring must have room, so wake every waiting connection or they could wait forever
			if rn.held == 0 {
				rn.rearmStarved(true)
			}
		} else {
			rn.br.Recycle(uint16(bid))
			rn.rearmStarved(rn.held == 0)
		}
	}
	if release != nil {
		release()
	}
}

// maybeRecycleInc returns an incremental buffer once the kernel is done with it and no slice is referenced.
func (rn *URingNet) maybeRecycleInc(bid uint16) {
	if rn.incDone[bid] && rn.incRefs[bid] == 0 {
		rn.incDone[bid] = false
		rn.br.Recycle(bid)
		// One large buffer holds data for many connections: wake all waiting connections
		rn.rearmStarved(true)
	}
}

// rearmStarved re-arms recv for connections stopped by -ENOBUFS when buffers come back.
// One connection per returned buffer, first in first out: waking all at once lets only a few get a buffer and
// the rest hit -ENOBUFS again (thundering herd). With all (no buffer held any more) it wakes all of them, or
// the remaining ones might never see another buffer returned.
func (rn *URingNet) rearmStarved(all bool) {
	for len(rn.starved) > 0 {
		c := rn.starved[0]
		rn.starved[0] = nil
		rn.starved = rn.starved[1:]
		if c.closed || c.recvActive || c.paused {
			continue
		}
		c.armRecv()
		if !all {
			break
		}
	}
	if len(rn.starved) == 0 {
		rn.starved = nil
	}
}

func sliceStart(b []byte) uintptr {
	return uintptr(unsafe.Pointer(unsafe.SliceData(b[:1])))
}

// overlaps reports whether the backing arrays of a and b (by capacity) overlap.
func overlaps(a, b []byte) bool {
	if cap(a) == 0 || cap(b) == 0 {
		return false
	}
	a0, b0 := sliceStart(a), sliceStart(b)
	return a0 < b0+uintptr(cap(b)) && b0 < a0+uintptr(cap(a))
}

// within reports whether a's data lies entirely within b's backing array (by capacity).
func within(a, b []byte) bool {
	if len(a) == 0 || cap(b) == 0 {
		return false
	}
	a0, b0 := sliceStart(a), sliceStart(b)
	return a0 >= b0 && a0+uintptr(len(a)) <= b0+uintptr(cap(b))
}
