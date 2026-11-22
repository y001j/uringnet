//go:build linux

// Package netaddr 把 "host:port" 解析成 IPv4 socket 地址，供直接用 unix.Bind 的压测服务端使用。
package netaddr

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// Inet4 解析 "host:port"（host 可为空、0.0.0.0、IP 或主机名）
func Inet4(addr string) (*unix.SockaddrInet4, error) {
	ta, err := net.ResolveTCPAddr("tcp4", addr)
	if err != nil {
		return nil, err
	}
	sa := &unix.SockaddrInet4{Port: ta.Port}
	if ip4 := ta.IP.To4(); ip4 != nil {
		copy(sa.Addr[:], ip4)
	} else if ta.IP != nil {
		return nil, fmt.Errorf("netaddr: %s is not an IPv4 address", addr)
	}
	return sa, nil
}
