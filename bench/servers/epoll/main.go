//go:build linux

// Hand-written epoll echo (baseline): one locked OS thread and its own SO_REUSEPORT listening socket per loop,
// non-blocking reads and writes, level-triggered; EPOLLOUT when a write is incomplete.
package main

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"

	"github.com/y001j/uringnet/bench/internal/netaddr"
)

type conn struct {
	out []byte // data not written yet
}

func listen(addr string) int {
	fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
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

func loop(addr string) {
	runtime.LockOSThread()
	lfd := listen(addr)
	ep, _ := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	unix.EpollCtl(ep, unix.EPOLL_CTL_ADD, lfd, &unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(lfd)})
	conns := map[int]*conn{}
	buf := make([]byte, 16384)
	events := make([]unix.EpollEvent, 512)
	closeConn := func(fd int) {
		unix.EpollCtl(ep, unix.EPOLL_CTL_DEL, fd, nil)
		unix.Close(fd)
		delete(conns, fd)
	}
	for {
		n, err := unix.EpollWait(ep, events, -1)
		if err != nil {
			continue
		}
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			ev := events[i].Events
			if fd == lfd {
				for {
					cfd, _, err := unix.Accept4(lfd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
					if err != nil {
						break
					}
					conns[cfd] = &conn{}
					unix.EpollCtl(ep, unix.EPOLL_CTL_ADD, cfd, &unix.EpollEvent{Events: unix.EPOLLIN | unix.EPOLLRDHUP, Fd: int32(cfd)})
				}
				continue
			}
			c := conns[fd]
			if c == nil {
				continue
			}
			if ev&unix.EPOLLOUT != 0 && len(c.out) > 0 {
				w, err := unix.Write(fd, c.out)
				if err != nil && err != unix.EAGAIN {
					closeConn(fd)
					continue
				}
				if w > 0 {
					c.out = c.out[w:]
				}
				if len(c.out) == 0 {
					unix.EpollCtl(ep, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{Events: unix.EPOLLIN | unix.EPOLLRDHUP, Fd: int32(fd)})
				}
				continue
			}
			if ev&(unix.EPOLLIN|unix.EPOLLRDHUP|unix.EPOLLHUP|unix.EPOLLERR) == 0 || len(c.out) > 0 {
				continue
			}
			r, err := unix.Read(fd, buf)
			if r <= 0 {
				if err != unix.EAGAIN {
					closeConn(fd)
				}
				continue
			}
			w, err := unix.Write(fd, buf[:r])
			if err != nil && err != unix.EAGAIN {
				closeConn(fd)
				continue
			}
			if w < 0 {
				w = 0
			}
			if w < r {
				c.out = append(c.out[:0], buf[w:r]...)
				unix.EpollCtl(ep, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{Events: unix.EPOLLOUT | unix.EPOLLRDHUP, Fd: int32(fd)})
			}
		}
	}
}

func main() {
	for i := 0; i < runtime.NumCPU(); i++ {
		go loop(os.Args[1])
	}
	select {}
}
