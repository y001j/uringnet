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
