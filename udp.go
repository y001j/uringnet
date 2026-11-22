//go:build linux
// +build linux

package uringnet

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	iouring_syscall "github.com/y001j/iouring-go/syscall"
	"golang.org/x/sys/unix"
)

// PacketHandler receives the datagrams of a PacketServer. OnPacket runs on the event loop
// of the socket that received the datagram and must not block.
type PacketHandler interface {
	// OnPacket is called once per datagram. data is only valid until OnPacket returns.
	// Reply with pc.WriteTo. Datagrams larger than a receive buffer are dropped and
	// counted in Stats.PacketsTruncated; size the buffers with WithBuffers.
	OnPacket(pc *PacketConn, data []byte, from netip.AddrPort)
}

// PacketHandlerFunc adapts a function to PacketHandler.
type PacketHandlerFunc func(pc *PacketConn, data []byte, from netip.AddrPort)

// OnPacket calls f.
func (f PacketHandlerFunc) OnPacket(pc *PacketConn, data []byte, from netip.AddrPort) {
	f(pc, data, from)
}

// PacketServer serves UDP: one socket per ring, with SO_REUSEPORT spreading datagrams over
// them. Bound to port 0 it also works as a UDP client through AsyncWriteTo.
type PacketServer struct {
	rings    []*URingNet
	loop     *Ringloop
	addr     net.Addr
	served   atomic.Bool
	stopOnce sync.Once
	next     atomic.Uint32
}

// PacketConn is one UDP socket of a PacketServer. WriteTo must be called on its event
// loop, that is from OnPacket.
type PacketConn struct {
	rn *URingNet
}

// ListenPacket creates a UDP server on address; network is "udp", "udp4" or "udp6". The
// options that apply are WithRings, WithBuffers, WithSQSize, WithBusyPoll*, WithReusePort
// and WithLogger. Receive buffers must hold a whole datagram plus about 48 bytes of
// header: the default 16KB buffers fit any datagram up to 16KB.
func ListenPacket(network, address string, h PacketHandler, opts ...Option) (*PacketServer, error) {
	switch network {
	case "udp", "udp4", "udp6":
	default:
		return nil, fmt.Errorf("uringnet: unsupported network %q for ListenPacket", network)
	}
	if h == nil {
		return nil, fmt.Errorf("uringnet: nil PacketHandler")
	}
	o := options{rings: runtime.NumCPU(), sqSize: defaultSQSize, bufCount: 256, bufSize: DefaultBufferSize, reusePort: true}
	for _, opt := range opts {
		opt(&o)
	}
	if o.rings <= 0 {
		return nil, fmt.Errorf("uringnet: invalid number of rings %d", o.rings)
	}
	reuse := o.reusePort && o.rings > 1
	s := &PacketServer{}
	log := o.logger
	if log == nil {
		log = discardLogger
	}
	// 开启 SO_REUSEPORT 时每个 ring 有自己的 socket（由 ring 关闭）；否则共用一个，由 Ringloop 关闭一次
	sharedFd := -1
	for i := 0; i < o.rings; i++ {
		fd := sharedFd
		if reuse || i == 0 {
			var err error
			if fd, err = listenUDP(network, address, reuse); err != nil {
				s.closeSockets()
				if sharedFd >= 0 {
					unix.Close(sharedFd)
				}
				return nil, err
			}
			if i == 0 {
				address = boundAddress(address, fd) // 端口为 0 时其余 socket 绑定同一个端口
				if !reuse {
					sharedFd = fd
				}
			}
		}
		rn := &URingNet{SocketFd: fd, index: i, entries: uint32(o.sqSize), ownsSocket: reuse, packet: true,
			BufferSize: o.bufSize, SpinTime: o.spin, SpinFixed: o.spinFixed, SpinBusyBatch: o.spinBusy,
			SpinIdleBatch: o.spinIdle, Logger: log, RecvBundle: BundleOff, Backend: o.backend}
		rn.h = packetHandler{}
		rn.ph = h
		rn.pc.rn = rn
		s.rings = append(s.rings, rn)
	}
	if sa, err := unix.Getsockname(s.rings[0].SocketFd); err == nil {
		s.addr = udpAddr(sa)
	}
	loop, err := SetLoops(s.rings, o.bufCount)
	if err != nil {
		s.closeSockets()
		if sharedFd >= 0 {
			unix.Close(sharedFd)
		}
		return nil, err
	}
	s.loop = loop
	return s, nil
}

func (s *PacketServer) closeSockets() {
	for _, rn := range s.rings {
		if rn.ownsSocket {
			unix.Close(rn.SocketFd)
		}
	}
}

// Addr returns the local address (with the actual port when bound to port 0).
func (s *PacketServer) Addr() net.Addr { return s.addr }

// Backend returns the backend the event loops use: BackendIOUring or BackendEpoll.
func (s *PacketServer) Backend() Backend { return s.rings[0].backend }

// Serve starts receiving and blocks until Close; it returns ErrServerClosed.
func (s *PacketServer) Serve() error {
	if !s.served.CompareAndSwap(false, true) {
		return fmt.Errorf("uringnet: Serve called twice or after Close")
	}
	s.loop.RunMany()
	s.loop.Wait()
	return ErrServerClosed
}

// Start starts receiving without blocking, for a PacketServer used as a client.
func (s *PacketServer) Start() error {
	if !s.served.CompareAndSwap(false, true) {
		return fmt.Errorf("uringnet: already started or closed")
	}
	s.loop.RunMany()
	return nil
}

// Close closes the sockets and event loops. Datagrams still queued for sending may be lost.
func (s *PacketServer) Close() error {
	s.stopOnce.Do(func() {
		s.served.Store(true)
		s.loop.Shutdown()
	})
	return nil
}

// AsyncWriteTo copies b and sends it to addr from one of the server's sockets. It is safe
// to call from any goroutine; done (may be nil) is called on the event loop with the
// result of the send.
func (s *PacketServer) AsyncWriteTo(b []byte, addr netip.AddrPort, done func(error)) error {
	rn := s.rings[int(s.next.Add(1))%len(s.rings)]
	return rn.pc.AsyncWriteTo(b, addr, done)
}

// Stats returns the server's counters (Accepted and the TCP-only counters stay zero).
func (s *PacketServer) Stats() Stats {
	return (&Server{rings: s.rings}).Stats()
}

// LocalAddr returns the socket's local address.
func (pc *PacketConn) LocalAddr() net.Addr {
	sa, err := unix.Getsockname(pc.rn.SocketFd)
	if err != nil {
		return nil
	}
	return udpAddr(sa)
}

// WriteTo sends b to addr. b is copied, so it can be reused once WriteTo returns. It must
// be called on the socket's event loop (from OnPacket); use AsyncWriteTo elsewhere.
func (pc *PacketConn) WriteTo(b []byte, addr netip.AddrPort) error {
	return pc.rn.sendTo(b, addr, nil)
}

// AsyncWriteTo copies b and sends it to addr from the socket's event loop. It is safe to
// call from any goroutine; done (may be nil) is called on the event loop with the result.
func (pc *PacketConn) AsyncWriteTo(b []byte, addr netip.AddrPort, done func(error)) error {
	buf := append([]byte(nil), b...)
	return pc.rn.Post(func() {
		if err := pc.rn.sendTo(buf, addr, done); err != nil && done != nil {
			done(err)
		}
	})
}

// packetHandler 让 UDP 的 ring 复用 URingNet 的启动与关闭流程；没有连接，这些回调都不会被调用
type packetHandler struct{}

func (packetHandler) boot(*URingNet)        {}
func (packetHandler) open(*conn)            {}
func (packetHandler) traffic(*conn, []byte) {}
func (packetHandler) written(*conn)         {}
func (packetHandler) closed(*conn)          {}
func (packetHandler) shutdown(*URingNet)    {}

// recvmsg 时为地址预留的空间：足够放下 sockaddr_in6
const packetNameLen = uint32(unsafe.Sizeof(unix.RawSockaddrInet6{}))

// recvmsgOutLen 是 struct io_uring_recvmsg_out 的大小：namelen、controllen、payloadlen、flags 四个 u32
const recvmsgOutLen = 16

// armRecvMsg 提交 multishot recvmsg（Linux 6.0+）：每个数据报占一个 provided buffer，
// 缓冲区内依次是 io_uring_recvmsg_out、地址（packetNameLen 字节）和数据
func (rn *URingNet) armRecvMsg() {
	if rn.stopping {
		return
	}
	rn.recvMsg = unix.Msghdr{Namelen: packetNameLen}
	rn.loop.RecvMsgMultishot(rn.SocketFd, &rn.recvMsg, rn.onRecvMsg)
}

func (rn *URingNet) onRecvMsg(res int32, flags uint32) {
	if bid, buf, ok := rn.br.Get(res, flags); ok {
		if res > 0 {
			rn.deliverPacket(buf)
		}
		rn.br.Recycle(bid)
	}
	if flags&iouring_syscall.IORING_CQE_F_MORE != 0 || rn.stopping {
		return
	}
	// multishot 已终止：缓冲区耗尽（数据报已在回调后立即归还）或出错时重新提交；
	// 持续出错时稍等再提交，避免空转
	if res >= 0 || res == -int32(syscall.ENOBUFS) {
		if res < 0 {
			rn.enobufs++
		}
		rn.armRecvMsg()
		return
	}
	rn.log.Warn("uringnet: UDP receive failed, retrying", "ring", rn.index, "err", syscall.Errno(-res))
	rn.after(10*time.Millisecond, rn.armRecvMsg)
}

// deliverPacket 解析 recvmsg 写入缓冲区的内容并调用 OnPacket
func (rn *URingNet) deliverPacket(buf []byte) {
	if len(buf) < recvmsgOutLen+int(packetNameLen) {
		return
	}
	nameLen := binary.NativeEndian.Uint32(buf[0:])
	payloadLen := binary.NativeEndian.Uint32(buf[8:])
	msgFlags := binary.NativeEndian.Uint32(buf[12:])
	payload := buf[recvmsgOutLen+int(packetNameLen):]
	if msgFlags&unix.MSG_TRUNC != 0 || int(payloadLen) > len(payload) {
		rn.truncated++
		rn.log.Debug("uringnet: dropping datagram larger than the receive buffer", "ring", rn.index, "size", payloadLen)
		return
	}
	payload = payload[:payloadLen]
	from := parseSockaddr(buf[recvmsgOutLen : recvmsgOutLen+int(min(nameLen, packetNameLen))])
	rn.packetsIn++
	rn.bytesIn += uint64(payloadLen)
	rn.ph.OnPacket(&rn.pc, payload, from)
}

// parseSockaddr 把内核写出的 sockaddr_in / sockaddr_in6 转为 netip.AddrPort（不分配内存）
func parseSockaddr(b []byte) netip.AddrPort {
	if len(b) < 4 {
		return netip.AddrPort{}
	}
	port := binary.BigEndian.Uint16(b[2:])
	switch binary.NativeEndian.Uint16(b) {
	case unix.AF_INET:
		if len(b) >= 8 {
			return netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), port)
		}
	case unix.AF_INET6:
		if len(b) >= 24 {
			return netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[8:24])), port)
		}
	}
	return netip.AddrPort{}
}

// sendTo 在 Loop 线程中用 sendmsg 发送一个数据报。消息头、iovec、地址和数据放在同一块内存里，
// 作为请求的 keep 保持存活直到发送完成
func (rn *URingNet) sendTo(b []byte, to netip.AddrPort, done func(error)) error {
	if rn.stopping {
		return ErrServerClosed
	}
	if !to.IsValid() {
		return fmt.Errorf("uringnet: invalid destination address")
	}
	var raw []byte
	addr := to.Addr()
	if addr.Is4() && rn.family == unix.AF_INET {
		var sa unix.RawSockaddrInet4
		sa.Family = unix.AF_INET
		putPort(&sa.Port, int(to.Port()))
		sa.Addr = addr.As4()
		raw = unsafe.Slice((*byte)(unsafe.Pointer(&sa)), unsafe.Sizeof(sa))
	} else {
		if rn.family == unix.AF_INET {
			return fmt.Errorf("uringnet: cannot send to %v from an IPv4 socket", to)
		}
		var sa unix.RawSockaddrInet6
		sa.Family = unix.AF_INET6
		putPort(&sa.Port, int(to.Port()))
		sa.Addr = addr.As16() // IPv4 地址按 IPv4 映射地址发送
		raw = unsafe.Slice((*byte)(unsafe.Pointer(&sa)), unsafe.Sizeof(sa))
	}
	const align = 8
	round := func(n uintptr) uintptr { return (n + align - 1) &^ (align - 1) }
	hdrOff := uintptr(0)
	iovOff := round(hdrOff + unsafe.Sizeof(unix.Msghdr{}))
	nameOff := round(iovOff + unsafe.Sizeof(unix.Iovec{}))
	dataOff := round(nameOff + uintptr(len(raw)))
	mem := make([]byte, dataOff+uintptr(len(b))+1) // +1：空数据报时 iovec 也指向有效内存
	copy(mem[nameOff:], raw)
	copy(mem[dataOff:], b)
	msg := (*unix.Msghdr)(unsafe.Pointer(&mem[hdrOff]))
	iov := (*unix.Iovec)(unsafe.Pointer(&mem[iovOff]))
	iov.Base = &mem[dataOff]
	iov.SetLen(len(b))
	msg.Name = &mem[nameOff]
	msg.Namelen = uint32(len(raw))
	msg.Iov = iov
	msg.SetIovlen(1)
	n := len(b)
	rn.loop.Sendmsg(rn.SocketFd, msg, mem, func(res int32, _ uint32) {
		var err error
		if res < 0 {
			err = syscall.Errno(-res)
			rn.sendErrs++
		} else {
			rn.packetsOut++
			rn.bytesOut += uint64(n)
		}
		if done != nil {
			done(err)
		}
	})
	return nil
}

// listenUDP 创建并绑定一个阻塞模式的 UDP socket（io_uring 负责异步等待）
func listenUDP(network, address string, reusePort bool) (int, error) {
	ua, err := net.ResolveUDPAddr(network, address)
	if err != nil {
		return -1, err
	}
	family := unix.AF_INET6
	if network == "udp4" || (network == "udp" && (ua.IP == nil || ua.IP.To4() != nil) && !ipv6Only(ua)) {
		family = unix.AF_INET
	}
	fd, err := unix.Socket(family, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("uringnet: udp socket: %w", err)
	}
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	if reusePort {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("uringnet: SO_REUSEPORT: %w", err)
		}
	}
	var sa unix.Sockaddr
	if family == unix.AF_INET {
		s4 := &unix.SockaddrInet4{Port: ua.Port}
		if ip4 := ua.IP.To4(); ip4 != nil {
			copy(s4.Addr[:], ip4)
		}
		sa = s4
	} else {
		s6 := &unix.SockaddrInet6{Port: ua.Port}
		if ua.IP != nil {
			copy(s6.Addr[:], ua.IP.To16())
		}
		if ua.Zone != "" {
			if ifi, err := net.InterfaceByName(ua.Zone); err == nil {
				s6.ZoneId = uint32(ifi.Index)
			}
		}
		if network == "udp6" {
			unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 1)
		}
		sa = s6
	}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("uringnet: bind %s: %w", address, err)
	}
	return fd, nil
}

func ipv6Only(ua *net.UDPAddr) bool { return ua.IP != nil && ua.IP.To4() == nil }

func udpAddr(sa unix.Sockaddr) net.Addr {
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		return &net.UDPAddr{IP: append(net.IP(nil), a.Addr[:]...), Port: a.Port}
	case *unix.SockaddrInet6:
		addr := &net.UDPAddr{IP: append(net.IP(nil), a.Addr[:]...), Port: a.Port}
		if a.ZoneId != 0 {
			addr.Zone = strconv.Itoa(int(a.ZoneId))
		}
		return addr
	}
	return nil
}
