//go:build linux
// +build linux

package uringnet

import (
	"syscall"
	"unsafe"

	iouring "github.com/y001j/iouring-go"
	iouring_syscall "github.com/y001j/iouring-go/syscall"
	"golang.org/x/sys/unix"
)

// seg 是发送队列中的一段数据
type seg struct {
	b       []byte
	bid     int32  // >= 0：b 位于该 provided buffer 中，发送完后归还
	release func() // 非 nil：b 由用户提供（WriteNoCopy），内核用完后调用
	copied  bool   // b 是框架的拷贝缓冲区（可以追加、发送完后复用）
	zc      *zcRef // 非 nil：使用 SEND_ZC 发送
}

// zcRef 跟踪一段零拷贝数据的在途通知：段发送完毕（done）且所有通知 CQE 到达（n == 0）后才能释放
type zcRef struct {
	n       int
	done    bool
	bid     int32
	release func()
}

// conn 是一个 TCP 连接的状态机，只在所属 URingNet 的 Loop 线程中访问。
//
//   - 读：一个 multishot recv 持续接收，数据落在 provided buffer ring 中；内核支持时使用
//     recv bundle，一个完成事件可携带多个缓冲区，逐个交给 OnTraffic。
//   - 写：发送队列按段组织，同一时刻最多一个 send 在途，保证字节流顺序；处理短写。
//     一次回调（bundle 时为一个完成事件中的全部缓冲区）产生的数据在回调结束后一并发送，
//     队列中的多段用一次 sendmsg 聚集发送（最多 maxIov 段）；拷贝的小数据合并到同一段中。
//   - 原地发送：WriteBuf 指向本次接收缓冲区（如 echo）且发送队列为空时，直接发送该缓冲区，发送完再归还。
//   - 零拷贝：开启 ZeroCopyThreshold 后，原地发送与 WriteNoCopy 的大段数据使用 SEND_ZC，
//     buffer ring 注册为固定缓冲区时使用 SendZCFixed；内存在通知 CQE 到达后才释放。
//   - 关闭：shutdown(fd) 使在途 recv 以 EOF 结束、在途 send 出错结束，
//     等 recv/send 都结束后才 close(fd)，避免 fd 被复用后串到新连接上。
type conn struct {
	ud UserData
	rn *URingNet
	fd int

	recvActive bool
	recvUD     uint64 // 在途 multishot recv 的 user_data（暂停接收时用于取消）
	paused     bool   // 发送队列超过 MaxWriteBuffer，暂停接收
	qBytes     int    // 发送队列中待发送的字节数
	lastActive uint64 // 最近一次收到或发出数据时的节拍号（IdleTimeout）
	lastSend   uint64 // 最近一次发送进展（或队列由空变为非空）时的节拍号（WriteTimeout）
	sending    bool
	q          []seg
	qBase      []seg // q 的底层数组起点，队列清空后复用，避免每次入队都分配
	off        int   // 队首段已发送的字节数
	inflight   int   // 在途 send 覆盖的队首段数
	iov        [maxIov]unix.Iovec
	msg        unix.Msghdr
	spare      [][]byte // 复用的拷贝缓冲区
	closeAfter bool     // 发送完成后关闭（EchoAndClose、Conn.Close）
	batching   bool     // 正在执行回调：入队的数据暂不发送，回调结束后由 flush 一并发出
	closed     bool     // 已 shutdown，等待在途请求结束
	freed      bool
	err        error // 关闭原因，交给 OnClose

	// 当前 OnTraffic 中的接收缓冲区：写入的数据位于其中时可以原地发送（curBid < 0 表示不在 OnTraffic 中）
	curBuf  []byte
	curBid  int32
	curHeld bool // 当前接收缓冲区被发送队列占用，回调结束后不能归还

	onRecvH iouring.Handler
	onSendH iouring.Handler
}

// maxIov 为一次聚集发送的最大段数
const maxIov = 16

func newConn(rn *URingNet, fd int) *conn {
	c := &conn{rn: rn, fd: fd, lastActive: rn.tick, lastSend: rn.tick, curBid: -1}
	c.ud.Fd = int32(fd)
	c.ud.c = c
	// 缓存方法值，避免每次提交请求都分配闭包
	c.onRecvH = c.onRecv
	c.onSendH = c.onSend
	return c
}

func (c *conn) armRecv() {
	c.recvActive = true
	if c.rn.bundle {
		c.recvUD = c.rn.loop.RecvMultishotBundle(c.fd, c.rn.br, 0, c.onRecvH)
	} else {
		c.recvUD = c.rn.loop.RecvMultishot(c.fd, c.rn.br, 0, c.onRecvH)
	}
}

// checkBackpressure 在回调结束后检查发送队列：超过上限时暂停接收（取消 multishot recv）。
// 取消完成前已经收到的数据仍会交给 OnTraffic。
func (c *conn) checkBackpressure() {
	max := c.rn.maxWrite
	if max <= 0 || c.paused || c.closed || c.qBytes <= max {
		return
	}
	c.paused = true
	c.rn.pauses++
	if c.recvActive {
		c.rn.loop.Cancel(c.recvUD, c.rn.nopH)
	}
}

// maybeResume 在发送取得进展后调用：队列降到上限的一半以下时恢复接收
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
	}
	c.batching = true
	if rn.bundle {
		// 一个 CQE 可能连续使用多个缓冲区，逐个交给 OnTraffic
		bufs := rn.br.Bundle(res, flags, rn.bundleBufs[:0])
		rn.bundleBufs = bufs[:0]
		for _, b := range bufs {
			c.deliver(b.Bid, b.Data)
		}
	} else if rn.inc {
		// 增量消费：数据位于大缓冲区中上一次数据之后；内核用完且没有片段被引用时才归还
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

	// multishot recv 已终止
	c.recvActive = false
	switch {
	case c.closed:
		c.maybeFree()
	case c.paused && (res > 0 || res == -int32(syscall.ECANCELED) || res == -int32(syscall.ENOBUFS)):
		// 背压暂停中：发送队列降下来后由 maybeResume 重新提交
	case res == -int32(syscall.ECANCELED):
		// 为暂停而取消，但取消完成前暂停已经解除
		c.armRecv()
	case res == -int32(syscall.ENOBUFS):
		// 缓冲区暂时耗尽。若有缓冲区被在途 send 占用，等它归还后再重新提交；
		// 否则所有已处理的缓冲区都已归还，下一次 io_uring_enter 会先发布再提交，可立即重试。
		if rn.held > 0 {
			rn.starved = append(rn.starved, c)
		} else {
			c.armRecv()
		}
	case res > 0:
		c.armRecv()
	default:
		// EOF（res == 0）或出错
		c.close()
	}
}

// deliver 把一个接收缓冲区中的数据交给 OnTraffic，没有被发送队列占用时立即归还
func (c *conn) deliver(bid uint16, data []byte) {
	held := c.traffic(bid, data)
	if !held {
		c.rn.br.Recycle(bid)
	}
}

// deliverInc 是增量消费模式下的 deliver：原地发送的片段在 writeFrom 中给缓冲区加引用，
// 缓冲区由调用方在内核用完且没有引用时归还
func (c *conn) deliverInc(bid uint16, data []byte) {
	c.traffic(bid, data)
}

// traffic 调用 OnTraffic，返回接收缓冲区是否被发送队列占用
func (c *conn) traffic(bid uint16, data []byte) (held bool) {
	if len(data) == 0 || c.closed {
		return false
	}
	c.curBuf, c.curBid, c.curHeld = data, int32(bid), false
	c.rn.h.traffic(c, data)
	held = c.curHeld
	c.curBuf, c.curBid, c.curHeld = nil, -1, false
	return held
}

// send 发送 b：在 OnTraffic 中且 b 位于本次接收缓冲区时尽量原地发送，否则拷贝
func (c *conn) send(b []byte) {
	if c.writeFrom(b, c.curBuf, c.curBid) {
		c.curHeld = true
	}
}

// closeAfterFlush 在发送队列清空后关闭连接；队列已空时立即关闭
func (c *conn) closeAfterFlush() {
	c.closeAfter = true
	if !c.sending && len(c.q) == 0 {
		c.close()
	}
}

// writeFrom 发送 b：发送队列为空且 b 位于本次接收缓冲区时原地发送（返回 true），否则拷贝。
// 只在队列为空时原地发送：允许一个连接同时占用多个接收缓冲区会加剧缓冲区耗尽（-ENOBUFS
// 后需重新提交 recv），实测得不偿失。
// 原地发送会占用接收缓冲区直到发送完成；被占用的缓冲区超过 maxHeld 时改为拷贝，
// 避免缓冲区耗尽：耗尽时 multishot recv 以 -ENOBUFS 终止，重新提交的开销远大于一次拷贝。
func (c *conn) writeFrom(b, recvBuf []byte, bid int32) bool {
	if len(b) == 0 || c.closed {
		return false
	}
	if bid >= 0 && c.canHold(uint16(bid)) && within(b, recvBuf) {
		rn := c.rn
		rn.held++
		if rn.inc {
			// 每个原地发送的片段给缓冲区加一个引用，releaseBuf 中逐个释放
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

// canHold 判断能否原地发送 bid 中的数据。
// 普通模式：只在发送队列为空时，且被占用的缓冲区少于 maxHeld。
// 增量消费模式：数据只占大缓冲区的一小段，允许一个连接原地发送多段；限制的是被引用的缓冲区数量
// （同一缓冲区中再多引用一段不会多占缓冲区）。
func (c *conn) canHold(bid uint16) bool {
	rn := c.rn
	if rn.inc {
		return rn.incRefs[bid] > 0 || rn.incPinned < rn.maxHeld
	}
	return len(c.q) == 0 && rn.held < rn.maxHeld
}

// write 把 b 拷贝进发送队列：尽量追加到队尾的拷贝段（在途 send 覆盖的段不能追加）
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

// writeNoCopy 把用户的 b 原样加入发送队列，内核用完后调用 release
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
		c.lastSend = rn.tick // 从这时起开始计算 WriteTimeout
	}
	c.q = append(c.q, s)
	c.qBytes += len(s.b)
	if len(c.q) == 1 {
		c.qBase = c.q // 从空队列开始时 q 位于底层数组起点
	}
	if !c.sending && !c.batching {
		c.off = 0
		c.sendHead()
	}
}

// flush 在回调结束后发送回调期间入队的数据
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
		if s.bid >= 0 && rn.fixedIdx >= 0 {
			rn.loop.SendZCFixed(c.fd, s.b[c.off:], uint16(rn.fixedIdx), unix.MSG_NOSIGNAL, 0, h)
		} else {
			rn.loop.SendZC(c.fd, s.b[c.off:], unix.MSG_NOSIGNAL, 0, h)
		}
		rn.zcSends++
		c.inflight = 1
		return
	}
	// 聚集发送：从队首起连续的非零拷贝段，最多 maxIov 段
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
		rn.loop.Send(c.fd, s.b[c.off:], unix.MSG_NOSIGNAL, c.onSendH)
		return
	}
	c.msg = unix.Msghdr{Iov: &c.iov[0]}
	c.msg.SetIovlen(n)
	rn.loop.Sendmsg(c.fd, &c.msg, unix.MSG_NOSIGNAL, c.onSendH)
	rn.gatherSends++
}

// zcHandler 处理 SEND_ZC 的结果 CQE 与通知 CQE
func (c *conn) zcHandler(ref *zcRef) iouring.Handler {
	return func(res int32, flags uint32) {
		if flags&iouring_syscall.IORING_CQE_F_NOTIF != 0 {
			ref.n--
			c.rn.maybeReleaseZC(ref)
			return
		}
		if flags&iouring_syscall.IORING_CQE_F_MORE == 0 {
			ref.n-- // 不会有通知 CQE
		}
		if res == -int32(syscall.EOPNOTSUPP) || res == -int32(syscall.EINVAL) || res == -int32(syscall.ENOMEM) {
			// 内核或 socket 不支持零拷贝（EOPNOTSUPP/EINVAL），或 pin 页超出 RLIMIT_MEMLOCK（ENOMEM）：
			// 这些条件不会很快改变，关闭本实例的零拷贝，用普通 send 重发
			c.rn.zcOff = true
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
		// 出错（含 shutdown 后的 -EPIPE / -ECANCELED）
		c.sending = false
		c.close()
		return
	}
	c.qBytes -= int(res)
	c.lastActive, c.lastSend = c.rn.tick, c.rn.tick
	// 按发送的字节数依次移除已发完的段；短写时停在未发完的段上
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

// popHead 移除已发送完的队首段并释放其内存
func (c *conn) popHead() {
	s := c.q[0]
	c.q[0] = seg{}
	c.q = c.q[1:]
	c.finish(s)
}

// finish 释放一段数据；零拷贝段要等所有通知 CQE 到达
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

// close 关闭连接：shutdown 让在途请求尽快结束，fd 在 maybeFree 中关闭。
// 可重复调用：已关闭时仍会尝试释放（在途请求结束的路径会再次调用它）。
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

// free 释放连接；正常路径只在没有在途 recv/send 时调用，Shutdown 时强制调用。
// 零拷贝段的内存仍会等通知 CQE 到达后释放（通知不依赖 fd）。
func (c *conn) free() {
	if c.freed {
		return
	}
	c.freed = true
	c.closed = true
	delete(c.rn.conns, c.fd)
	c.rn.connCount.Add(-1)
	unix.Close(c.fd)
	c.rn.h.closed(c)
	if !c.sending {
		for len(c.q) > 0 {
			c.popHead()
		}
	}
	c.spare = nil
}

// ---- URingNet 侧的缓冲区管理（Loop 线程）----

func (rn *URingNet) zcEnabled() bool { return rn.ZeroCopyThreshold > 0 && !rn.zcOff }

func (rn *URingNet) maybeReleaseZC(ref *zcRef) {
	if ref.done && ref.n == 0 {
		rn.releaseBuf(ref.bid, ref.release)
		ref.bid, ref.release = -1, nil
	}
}

// releaseBuf 归还被发送占用的接收缓冲区，或通知用户缓冲区可复用
func (rn *URingNet) releaseBuf(bid int32, release func()) {
	if bid >= 0 {
		rn.held--
		if rn.inc {
			b := uint16(bid)
			if rn.incRefs[b]--; rn.incRefs[b] == 0 {
				rn.incPinned--
				rn.maybeRecycleInc(b)
			}
			// 引用归零时缓冲区可能尚未被内核用完（不会归还、也就不会唤醒等待的连接）；
			// 没有任何片段被占用时，ring 中必然有可用空间，唤醒全部等待的连接，否则它们会永远等下去
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

// maybeRecycleInc 在内核已用完且没有片段引用时归还增量消费模式的缓冲区
func (rn *URingNet) maybeRecycleInc(bid uint16) {
	if rn.incDone[bid] && rn.incRefs[bid] == 0 {
		rn.incDone[bid] = false
		rn.br.Recycle(bid)
		// 一个大缓冲区能容纳许多连接的数据：唤醒全部等待的连接
		rn.rearmStarved(true)
	}
}

// rearmStarved 有缓冲区归还时，为因 -ENOBUFS 停止接收的连接重新提交 recv。
// 每归还一个缓冲区只唤醒一个连接（先进先出）：一次全部唤醒时只有少数能拿到缓冲区，
// 其余会立刻再次 -ENOBUFS（惊群）。all 为 true（已没有被占用的缓冲区）时唤醒全部，
// 否则剩下的连接可能再也等不到归还。
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

// overlaps 判断 a 与 b 的底层数组（按容量计算）是否有重叠
func overlaps(a, b []byte) bool {
	if cap(a) == 0 || cap(b) == 0 {
		return false
	}
	a0, b0 := sliceStart(a), sliceStart(b)
	return a0 < b0+uintptr(cap(b)) && b0 < a0+uintptr(cap(a))
}

// within 判断 a 的数据是否完全位于 b 的底层数组（按容量计算）内
func within(a, b []byte) bool {
	if len(a) == 0 || cap(b) == 0 {
		return false
	}
	a0, b0 := sliceStart(a), sliceStart(b)
	return a0 >= b0 && a0+uintptr(len(a)) <= b0+uintptr(cap(b))
}
