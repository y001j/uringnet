// Copyright (c) 2022 Rocky Yang
// Copyright (c) 2020 Andy Pan
// Copyright (c) 2017 Max Riveiro
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux || freebsd || dragonfly || darwin
// +build linux freebsd dragonfly darwin

// Package socket provides functions that return fd and net.Addr based on
// given the protocol and address with a SO_REUSEPORT option set to the socket.
package socket

import (
	"net"
	"strings"
	"time"
)

type NetAddressType string

const (
	Tcp  NetAddressType = "tcp"
	Tcp4 NetAddressType = "tcp4"
	Tcp6 NetAddressType = "tcp6"
	Udp  NetAddressType = "udp"
	Udp4 NetAddressType = "udp4"
	Udp6 NetAddressType = "udp6"
	Unix NetAddressType = "unix"
)

// Option is used for setting an option on socket.
type Option struct {
	SetSockOpt func(int, int) error
	Opt        int
}

// TCPSocket calls the internal tcpSocket.
func TCPSocket(proto, addr string, passive bool, sockOpts ...Option) (int, net.Addr, error) {
	return tcpSocket(proto, addr, passive, sockOpts...)
}

// UDPSocket calls the internal udpSocket.
func UDPSocket(proto, addr string, connect bool, sockOpts ...Option) (int, net.Addr, error) {
	return udpSocket(proto, addr, connect, sockOpts...)
}

// UnixSocket calls the internal udsSocket.
func UnixSocket(proto, addr string, passive bool, sockOpts ...Option) (int, net.Addr, error) {
	return udsSocket(proto, addr, passive, sockOpts...)
}

// TCPSocketOpt is the type of TCP socket options.
type TCPSocketOpt int

// Available TCP socket options.
const (
	TCPNoDelay TCPSocketOpt = iota
	TCPDelay
)

// Options are configurations for sockets creation.
type SocketOptions struct {
	// ================================== Options for only server-side ==================================

	// NumEventLoop is set up to start the given number of event-loop goroutine.
	// Note: Setting up NumEventLoop will override Multicore.
	NumEventLoop int

	// LB represents the load-balancing algorithm used when assigning new connections.
	//LB LoadBalancing

	// ReuseAddr indicates whether to set up the SO_REUSEADDR socket option.
	ReuseAddr bool

	// ReusePort indicates whether to set up the SO_REUSEPORT socket option.
	ReusePort bool

	// ============================= Options for both server-side and client-side =============================

	// ReadBufferCap is the maximum number of bytes that can be read from the peer when the readable event comes.
	// The default value is 64KB, it can either be reduced to avoid starving the subsequent connections or increased
	// to read more data from a socket.
	//
	// Note that ReadBufferCap will always be converted to the least power of two integer value greater than
	// or equal to its real amount.
	ReadBufferCap int

	// WriteBufferCap is the maximum number of bytes that a static outbound buffer can hold,
	// if the data exceeds this value, the overflow will be stored in the elastic linked list buffer.
	// The default value is 64KB.
	//
	// Note that WriteBufferCap will always be converted to the least power of two integer value greater than
	// or equal to its real amount.
	WriteBufferCap int

	// LockOSThread is used to determine whether each I/O event-loop is associated to an OS thread, it is useful when you
	// need some kind of mechanisms like thread local storage, or invoke certain C libraries (such as graphics lib: GLib)
	// that require thread-level manipulation via cgo, or want all I/O event-loops to actually run in parallel for a
	// potential higher performance.
	LockOSThread bool

	// Ticker indicates whether the ticker has been set up.
	Ticker bool

	// TCPKeepAlive sets up a duration for (SO_KEEPALIVE) socket option.
	TCPKeepAlive time.Duration

	// TCPNoDelay controls whether the operating system should delay
	// packet transmission in hopes of sending fewer packets (Nagle's algorithm).
	//
	// The default is true (no delay), meaning that data is sent
	// as soon as possible after a write operation.
	TCPNoDelay TCPSocketOpt

	// SocketRecvBuffer sets the maximum socket receive buffer in bytes.
	SocketRecvBuffer int

	// SocketSendBuffer sets the maximum socket send buffer in bytes.
	SocketSendBuffer int
}

func SetOptions(network string, options SocketOptions) []Option {
	var sockOpts []Option
	if options.ReusePort || strings.HasPrefix(network, "udp") {
		sockOpt := Option{SetSockOpt: SetReuseport, Opt: 1}
		sockOpts = append(sockOpts, sockOpt)
	}
	if options.ReuseAddr {
		sockOpt := Option{SetSockOpt: SetReuseAddr, Opt: 1}
		sockOpts = append(sockOpts, sockOpt)
	}
	if options.TCPNoDelay == TCPNoDelay && strings.HasPrefix(network, "tcp") {
		sockOpt := Option{SetSockOpt: SetNoDelay, Opt: 1}
		sockOpts = append(sockOpts, sockOpt)
	}
	if options.SocketRecvBuffer > 0 {
		sockOpt := Option{SetSockOpt: SetRecvBuffer, Opt: options.SocketRecvBuffer}
		sockOpts = append(sockOpts, sockOpt)
	}
	if options.SocketSendBuffer > 0 {
		sockOpt := Option{SetSockOpt: SetSendBuffer, Opt: options.SocketSendBuffer}
		sockOpts = append(sockOpts, sockOpt)
	}
	return sockOpts
}
