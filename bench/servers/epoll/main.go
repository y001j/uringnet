//go:build linux

// 手写 epoll echo（对照组）：每个 loop 一个锁定的 OS 线程 + 自己的 SO_REUSEPORT 监听 socket，
// 非阻塞读写，水平触发；写不完时挂 EPOLLOUT。
package main

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

type conn struct {
	out []byte // 未写完的数据
}

func listen(port int) int {
	fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	if err := unix.Bind(fd, &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		panic(err)
	}
	unix.Listen(fd, 4096)
	return fd
}

func loop(port int) {
	runtime.LockOSThread()
	lfd := listen(port)
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
	port := 0
	for _, ch := range os.Args[1][len("127.0.0.1:"):] {
		port = port*10 + int(ch-'0')
	}
	for i := 0; i < runtime.NumCPU(); i++ {
		go loop(port)
	}
	select {}
}
