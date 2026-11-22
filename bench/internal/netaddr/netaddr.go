//go:build linux

// Package netaddr parses "host:port" into an IPv4 socket address for benchmark servers that call unix.Bind directly.
package netaddr

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// Inet4 parses "host:port" (host may be empty, 0.0.0.0, an IP or a host name).
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
