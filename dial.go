//go:build linux
// +build linux

package uringnet

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Client is a set of event loops for outgoing connections only. Its connections use the
// same Handler callbacks as a Server's.
type Client struct {
	s *Server
}

// NewClient creates a client and starts its event loops. Listening options (WithReusePort)
// are ignored.
func NewClient(h Handler, opts ...Option) (*Client, error) {
	s, err := newServer("", "", h, opts)
	if err != nil {
		return nil, err
	}
	s.served.Store(true)
	s.h.OnBoot(s)
	s.loop.RunMany()
	return &Client{s: s}, nil
}

// Dial connects like Server.Dial.
func (cl *Client) Dial(ctx context.Context, network, address string, connCtx interface{}) (*Conn, error) {
	return cl.s.Dial(ctx, network, address, connCtx)
}

// DialAsync connects like Server.DialAsync.
func (cl *Client) DialAsync(network, address string, connCtx interface{}, done func(*Conn, error)) error {
	return cl.s.DialAsync(network, address, connCtx, done)
}

// Connections returns the number of open connections.
func (cl *Client) Connections() int64 { return cl.s.Connections() }

// Stats returns the client's counters.
func (cl *Client) Stats() Stats { return cl.s.Stats() }

// Shutdown closes the connections gracefully, like Server.Shutdown.
func (cl *Client) Shutdown(ctx context.Context) error { return cl.s.Shutdown(ctx) }

// Close closes all connections and event loops at once.
func (cl *Client) Close() error { return cl.s.Close() }

// Dial connects to address ("tcp", "tcp4", "tcp6" or "unix") from one of the server's event
// loops and returns the connection once it is established. OnOpen has run by then, with
// c.Context() already set to connCtx, so it can tell outgoing connections apart. ctx bounds
// the whole dial, including name resolution.
//
// Dial blocks, so it must not be called from a callback; use Conn.DialAsync or DialAsync there.
func (s *Server) Dial(ctx context.Context, network, address string, connCtx interface{}) (*Conn, error) {
	sa, domain, err := resolveDial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	rn := s.pickRing()
	results := make(chan result, 1)
	req := &dialReq{connCtx: connCtx, done: func(c *conn, err error) { results <- result{c, err} }}
	if err := rn.Post(func() { rn.connect(sa, domain, req) }); err != nil {
		return nil, ErrServerClosed
	}
	select {
	case r := <-results:
		return r.api(), r.err
	case <-ctx.Done():
	}
	// 在 Loop 线程中放弃这次拨号：连接还没建立时取消请求，之后的结果会被丢弃（关闭 fd，不调用 OnOpen）
	abandoned := make(chan bool, 1)
	if rn.Post(func() {
		if req.finished {
			abandoned <- false
			return
		}
		req.cancelled = true
		rn.loop.Cancel(req.ud)
		abandoned <- true
	}) != nil {
		return nil, ctx.Err()
	}
	if <-abandoned {
		return nil, ctx.Err()
	}
	r := <-results // 结果已经先一步到达
	return r.api(), r.err
}

// DialAsync connects like Dial without blocking: name resolution runs on a separate
// goroutine, and done (may be nil) is called on the new connection's event loop after
// OnOpen, or with an error if the connection could not be made.
func (s *Server) DialAsync(network, address string, connCtx interface{}, done func(*Conn, error)) error {
	return s.pickRing().dialAsync(network, address, connCtx, done)
}

// DialAsync connects from this connection's event loop, so the new connection and cn can
// write to each other directly from their callbacks (for example in a proxy). done (may be
// nil) is called on that event loop after the new connection's OnOpen, or with an error.
// It is safe to call from any goroutine.
func (cn *Conn) DialAsync(network, address string, connCtx interface{}, done func(*Conn, error)) error {
	return cn.c.rn.dialAsync(network, address, connCtx, done)
}

func (rn *URingNet) dialAsync(network, address string, connCtx interface{}, done func(*Conn, error)) error {
	if rn.loop == nil {
		return ErrServerClosed
	}
	finish := func(c *conn, err error) {
		if done != nil {
			done(result{c, err}.api(), err)
		}
	}
	go func() {
		sa, domain, err := resolveDial(context.Background(), network, address)
		if err != nil {
			rn.Post(func() { finish(nil, err) })
			return
		}
		req := &dialReq{connCtx: connCtx, done: finish}
		if rn.Post(func() { rn.connect(sa, domain, req) }) != nil && done != nil {
			done(nil, ErrServerClosed) // 事件循环已经关闭，没有别的线程可用了
		}
	}()
	return nil
}

type result struct {
	c   *conn
	err error
}

func (r result) api() *Conn {
	if r.c == nil {
		return nil
	}
	return &r.c.api
}

// dialReq 是一次进行中的拨号，字段只在 Loop 线程中访问
type dialReq struct {
	connCtx   interface{}
	done      func(*conn, error)
	ud        uint64 // connect 请求的 user_data，用于取消
	finished  bool   // connect 已完成
	cancelled bool   // 调用方已放弃：结果丢弃
}

var dialRing atomic.Uint32

func (s *Server) pickRing() *URingNet {
	return s.rings[int(dialRing.Add(1))%len(s.rings)]
}

// connect 在 Loop 线程中发起 IORING_OP_CONNECT
func (rn *URingNet) connect(sa unix.Sockaddr, domain int, req *dialReq) {
	if rn.stopping || rn.draining {
		req.finished = true
		req.done(nil, ErrServerClosed)
		return
	}
	fd, err := unix.Socket(domain, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		req.finished = true
		req.done(nil, err)
		return
	}
	if domain != unix.AF_UNIX && rn.noDelay {
		unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	}
	raw, err := rawSockaddr(sa)
	if err != nil {
		unix.Close(fd)
		req.finished = true
		req.done(nil, err)
		return
	}
	req.ud = rn.loop.Connect(fd, raw, func(res int32, _ uint32) {
		req.finished = true
		switch {
		case req.cancelled:
			unix.Close(fd)
		case res < 0:
			unix.Close(fd)
			req.done(nil, syscall.Errno(-res))
		case rn.stopping || rn.draining:
			unix.Close(fd)
			req.done(nil, ErrServerClosed)
		default:
			rn.dialed++
			c := rn.openConn(fd, req.connCtx)
			req.done(c, nil) // OnOpen 可能已经关闭了连接；之后的写入会返回 ErrConnClosed
		}
	})
}

// resolveDial 把地址解析为 sockaddr 和协议族
func resolveDial(ctx context.Context, network, address string) (unix.Sockaddr, int, error) {
	switch network {
	case "unix":
		return &unix.SockaddrUnix{Name: address}, unix.AF_UNIX, nil
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, 0, fmt.Errorf("uringnet: unsupported network %q for Dial", network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, 0, err
	}
	portNum, err := net.DefaultResolver.LookupPort(ctx, network, port)
	if err != nil {
		return nil, 0, err
	}
	var ip net.IP
	var zone string
	if host == "" {
		ip = net.IPv4(127, 0, 0, 1)
		if network == "tcp6" {
			ip = net.IPv6loopback
		}
	} else if ip = net.ParseIP(host); ip == nil {
		h, z, _ := cutZone(host)
		if ip = net.ParseIP(h); ip == nil {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, 0, err
			}
			for _, a := range addrs {
				if (network == "tcp4" && a.IP.To4() == nil) || (network == "tcp6" && a.IP.To4() != nil) {
					continue
				}
				ip, zone = a.IP, a.Zone
				break
			}
			if ip == nil {
				return nil, 0, fmt.Errorf("uringnet: no %s address for %s", network, host)
			}
		} else {
			zone = z
		}
	}
	if ip4 := ip.To4(); ip4 != nil && network != "tcp6" {
		sa := &unix.SockaddrInet4{Port: portNum}
		copy(sa.Addr[:], ip4)
		return sa, unix.AF_INET, nil
	}
	if network == "tcp4" {
		return nil, 0, fmt.Errorf("uringnet: %s is not an IPv4 address", host)
	}
	sa := &unix.SockaddrInet6{Port: portNum}
	copy(sa.Addr[:], ip.To16())
	if zone != "" {
		if ifi, err := net.InterfaceByName(zone); err == nil {
			sa.ZoneId = uint32(ifi.Index)
		}
	}
	return sa, unix.AF_INET6, nil
}

func cutZone(host string) (string, string, bool) {
	for i := 0; i < len(host); i++ {
		if host[i] == '%' {
			return host[:i], host[i+1:], true
		}
	}
	return host, "", false
}

// rawSockaddr 把 sockaddr 编码为内核的 struct sockaddr_*（connect 需要指针和长度）
func rawSockaddr(sa unix.Sockaddr) ([]byte, error) {
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		var raw unix.RawSockaddrInet4
		raw.Family = unix.AF_INET
		putPort(&raw.Port, a.Port)
		raw.Addr = a.Addr
		return structBytes(unsafe.Pointer(&raw), unsafe.Sizeof(raw)), nil
	case *unix.SockaddrInet6:
		var raw unix.RawSockaddrInet6
		raw.Family = unix.AF_INET6
		putPort(&raw.Port, a.Port)
		raw.Addr = a.Addr
		raw.Scope_id = a.ZoneId
		return structBytes(unsafe.Pointer(&raw), unsafe.Sizeof(raw)), nil
	case *unix.SockaddrUnix:
		var raw unix.RawSockaddrUnix
		if len(a.Name) >= len(raw.Path) {
			return nil, fmt.Errorf("uringnet: unix socket path too long: %q", a.Name)
		}
		raw.Family = unix.AF_UNIX
		for i := 0; i < len(a.Name); i++ {
			raw.Path[i] = int8(a.Name[i])
		}
		n := uintptr(2 + len(a.Name)) // sa_family + 路径
		if len(a.Name) > 0 && a.Name[0] == '@' {
			raw.Path[0] = 0 // 抽象命名空间：不含结尾的 0
		} else {
			n++ // 普通路径：包含结尾的 0
		}
		return structBytes(unsafe.Pointer(&raw), n), nil
	}
	return nil, fmt.Errorf("uringnet: unsupported address %T", sa)
}

// putPort 以网络字节序写入端口
func putPort(p *uint16, port int) {
	b := (*[2]byte)(unsafe.Pointer(p))
	b[0], b[1] = byte(port>>8), byte(port)
}

func structBytes(p unsafe.Pointer, n uintptr) []byte {
	return append([]byte(nil), unsafe.Slice((*byte)(p), n)...)
}
