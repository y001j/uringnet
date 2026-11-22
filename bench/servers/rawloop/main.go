//go:build linux

// 直接基于 iouring.Loop 的最简 echo 服务，作为 uringnet 封装开销的基线。
// 环境变量：BUFS（缓冲区数量，默认 1024）、BUFSIZE（默认 16384）、NOSI=1（关闭 SINGLE_ISSUER/DEFER_TASKRUN）
package main

import (
	"os"
	"strconv"
	"runtime"
	"syscall"

	iouring "github.com/y001j/iouring-go"
	iouring_syscall "github.com/y001j/iouring-go/syscall"
	"golang.org/x/sys/unix"

	"github.com/y001j/uringnet/bench/internal/netaddr"
)

type chunk struct {
	bid  uint16
	data []byte
}

type conn struct {
	l       *iouring.Loop
	br      *iouring.BufferRing
	fd      int
	q       []chunk
	off     int
	sending bool
	recv    bool
	closed  bool
	onRecvH iouring.Handler
	onSendH iouring.Handler
}

func (c *conn) arm() { c.recv = true; c.l.RecvMultishot(c.fd, c.br, 0, c.onRecvH) }

func (c *conn) onRecv(res int32, flags uint32) {
	if bid, data, ok := c.br.Get(res, flags); ok {
		if res > 0 && !c.closed {
			c.q = append(c.q, chunk{bid, data})
			if !c.sending {
				c.send()
			}
		} else {
			c.br.Recycle(bid)
		}
	}
	if flags&iouring_syscall.IORING_CQE_F_MORE != 0 {
		return
	}
	c.recv = false
	switch {
	case c.closed:
		c.free()
	case res > 0 || res == -int32(syscall.ENOBUFS):
		c.arm()
	default:
		c.close()
	}
}

func (c *conn) send() {
	c.sending = true
	c.l.Send(c.fd, c.q[0].data[c.off:], unix.MSG_NOSIGNAL, c.onSendH)
}

func (c *conn) onSend(res int32, flags uint32) {
	if res <= 0 || c.closed {
		c.sending = false
		c.close()
		return
	}
	c.off += int(res)
	if c.off < len(c.q[0].data) {
		c.send()
		return
	}
	c.br.Recycle(c.q[0].bid)
	c.q = c.q[1:]
	c.off = 0
	if len(c.q) > 0 {
		c.send()
		return
	}
	c.sending = false
}

func (c *conn) close() {
	if c.closed {
		return
	}
	c.closed = true
	unix.Shutdown(c.fd, unix.SHUT_RDWR)
	c.free()
}

func (c *conn) free() {
	if c.recv || c.sending {
		return
	}
	for _, ch := range c.q {
		c.br.Recycle(ch.bid)
	}
	c.q = nil
	unix.Close(c.fd)
}

func listen(addr string) int {
	fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	sa, err := netaddr.Inet4(addr)
	if err != nil {
		panic(err)
	}
	if err := unix.Bind(fd, sa); err != nil {
		panic(err)
	}
	unix.Listen(fd, 4096)
	return fd
}

func main() {
	addr := os.Args[1]
	count, size := 1024, 16384
	if v, err := strconv.Atoi(os.Getenv("BUFS")); err == nil {
		count = v
	}
	if v, err := strconv.Atoi(os.Getenv("BUFSIZE")); err == nil {
		size = v
	}
	for i := 0; i < runtime.NumCPU(); i++ {
		l, err := iouring.NewLoop(iouring.LoopConfig{Entries: 4096, DisableSingleIssuer: os.Getenv("NOSI") == "1",
			BufferRings: []iouring.BufferRingConfig{{Count: count, Size: size}}})
		if err != nil {
			panic(err)
		}
		br := l.BufferRing(0)
		lfd := listen(addr)
		var onAccept iouring.Handler
		onAccept = func(res int32, flags uint32) {
			if res >= 0 {
				c := &conn{l: l, br: br, fd: int(res)}
				c.onRecvH, c.onSendH = c.onRecv, c.onSend
				c.arm()
			}
			if flags&iouring_syscall.IORING_CQE_F_MORE == 0 {
				l.AcceptMultishot(lfd, unix.SOCK_CLOEXEC, onAccept)
			}
		}
		l.Post(func() { l.AcceptMultishot(lfd, unix.SOCK_CLOEXEC, onAccept) })
	}
	select {}
}
