//go:build linux
// +build linux

package uringnet

import (
	"syscall"
	"time"
	"unsafe"

	iouring_syscall "github.com/y001j/iouring-go/syscall"
)

// kernelTimespec 与内核的 struct __kernel_timespec 布局一致（在 32 位平台上也是两个 int64）
type kernelTimespec struct {
	sec  int64
	nsec int64
}

// 超时检查的节拍间隔为最短超时的 1/4，限制在 [minTick, maxTick] 内：
// 连接在超时后的 1 到 1.25 倍时间内被关闭
const (
	minTick = 5 * time.Millisecond
	maxTick = time.Second
)

// setupTimeouts 根据 IdleTimeout/WriteTimeout 计算节拍间隔，在 start 中调用
func (rn *URingNet) setupTimeouts() {
	shortest := time.Duration(0)
	for _, d := range []time.Duration{rn.IdleTimeout, rn.WriteTimeout} {
		if d > 0 && (shortest == 0 || d < shortest) {
			shortest = d
		}
	}
	if shortest == 0 {
		return
	}
	rn.tickEvery = shortest / 4
	if rn.tickEvery < minTick {
		rn.tickEvery = minTick
	} else if rn.tickEvery > maxTick {
		rn.tickEvery = maxTick
	}
	// 活动发生在某个节拍之后的任意时刻，所以多算一个节拍，保证至少经过了超时时间
	ticks := func(d time.Duration) uint64 {
		if d <= 0 {
			return 0
		}
		return uint64((d+rn.tickEvery-1)/rn.tickEvery) + 1
	}
	rn.idleTicks, rn.writeTicks = ticks(rn.IdleTimeout), ticks(rn.WriteTimeout)
}

// startTimeouts 在 Loop 线程中启动超时检查（RunMany 时投递）
func (rn *URingNet) startTimeouts() {
	if rn.tickEvery > 0 {
		rn.after(rn.tickEvery, rn.onTick)
	}
}

// onTick 推进节拍并关闭超时的连接
func (rn *URingNet) onTick() {
	if rn.stopping {
		return
	}
	rn.tick++
	for _, c := range rn.conns {
		if c.closed {
			continue
		}
		if rn.idleTicks > 0 && rn.tick-c.lastActive >= rn.idleTicks {
			rn.timeouts++
			rn.log.Debug("uringnet: closing idle connection", "fd", c.fd)
			c.fail(ErrIdleTimeout)
			continue
		}
		if rn.writeTicks > 0 && len(c.q) > 0 && rn.tick-c.lastSend >= rn.writeTicks {
			rn.timeouts++
			rn.log.Debug("uringnet: closing connection, send queue stalled", "fd", c.fd, "queued", c.qBytes)
			c.fail(ErrWriteTimeout)
		}
	}
	rn.after(rn.tickEvery, rn.onTick)
}

// after 提交一个一次性的 IORING_OP_TIMEOUT，到期后在 Loop 线程中执行 fn。只能在 Loop 线程中调用。
// 定时器被取消（ring 关闭）时不执行 fn。
func (rn *URingNet) after(d time.Duration, fn func()) {
	if d <= 0 {
		d = time.Nanosecond
	}
	buf := make([]byte, unsafe.Sizeof(kernelTimespec{}))
	ts := (*kernelTimespec)(unsafe.Pointer(&buf[0]))
	ts.sec = int64(d / time.Second)
	ts.nsec = int64(d % time.Second)
	rn.loop.Prep(func(sqe *iouring_syscall.SubmissionQueueEntry) {
		// len = 1（一个 timespec），off = 0：只在到期时完成，不按完成事件数计数
		sqe.PrepOperation(iouring_syscall.IORING_OP_TIMEOUT, -1, uint64(uintptr(unsafe.Pointer(&buf[0]))), 1, 0)
	}, buf, func(res int32, _ uint32) {
		if res == -int32(syscall.ETIME) {
			fn()
		}
	})
}
