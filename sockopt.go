//go:build linux
// +build linux

package uringnet

import (
	"time"

	"golang.org/x/sys/unix"
)

// keepAliveProbes is the number of unanswered keepalive probes after which the kernel drops the connection.
const keepAliveProbes = 9

// setKeepAlive enables TCP keepalive on fd: the first probe after d of idleness, then every d.
func setKeepAlive(fd int, d time.Duration) error {
	secs := int(d / time.Second)
	if secs < 1 {
		secs = 1
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1); err != nil {
		return err
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_KEEPIDLE, secs); err != nil {
		return err
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_KEEPINTVL, secs); err != nil {
		return err
	}
	return unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_KEEPCNT, keepAliveProbes)
}

// setSocketBuffers sets SO_RCVBUF / SO_SNDBUF on fd; 0 leaves a size unchanged.
func setSocketBuffers(fd, recv, send int) {
	if recv > 0 {
		unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, recv)
	}
	if send > 0 {
		unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, send)
	}
}
