//go:build linux
// +build linux

package uringnet

import (
	"crypto/tls"
	stderrors "errors"
	"net"

	"golang.org/x/sys/unix"
)

var (
	// ErrConnClosed is returned when writing to a connection that is closed or closing.
	ErrConnClosed = stderrors.New("uringnet: connection closed")
	// ErrServerClosed is returned by Server.Serve after Shutdown or Close, and passed to
	// OnClose for connections closed because the server shut down.
	ErrServerClosed = stderrors.New("uringnet: server closed")
	// ErrIdleTimeout is passed to OnClose for connections closed by the idle timeout.
	ErrIdleTimeout = stderrors.New("uringnet: idle timeout")
	// ErrWriteTimeout is passed to OnClose for connections closed because their send
	// queue made no progress within the write timeout.
	ErrWriteTimeout = stderrors.New("uringnet: write timeout")
)

// errConnClosed is the old internal name.
var errConnClosed = ErrConnClosed

// Handler receives the events of a Server. All methods run on the event loop thread
// that owns the connection and must not block. Embed BuiltinHandler to implement only
// the methods you need.
type Handler interface {
	// OnBoot is called once, when the server starts serving.
	OnBoot(s *Server)

	// OnOpen is called when a connection is accepted. Data written to c here is sent
	// once OnOpen returns.
	OnOpen(c *Conn) Action

	// OnTraffic is called when data arrives. data is only valid until OnTraffic returns:
	// its memory goes back to the kernel afterwards, so copy what you need to keep.
	// Writing data (or a slice of it) back with c.Write sends it in place, without a copy.
	OnTraffic(c *Conn, data []byte) Action

	// OnClose is called exactly once per connection, after it is closed. err is nil when
	// the connection was closed with Conn.Close or the Close action, io.EOF when the peer
	// closed it, ErrIdleTimeout/ErrWriteTimeout for timeouts, ErrServerClosed on shutdown,
	// and the socket error (a syscall.Errno) otherwise.
	OnClose(c *Conn, err error)

	// OnShutdown is called once, after all connections and event loops are closed.
	OnShutdown(s *Server)
}

// BuiltinHandler implements every Handler method with no-ops.
type BuiltinHandler struct{}

func (BuiltinHandler) OnBoot(*Server)                 {}
func (BuiltinHandler) OnOpen(*Conn) Action            { return None }
func (BuiltinHandler) OnTraffic(*Conn, []byte) Action { return None }
func (BuiltinHandler) OnClose(*Conn, error)           {}
func (BuiltinHandler) OnShutdown(*Server)             {}

// Conn is a connection, valid from OnOpen until OnClose returns.
//
// Methods other than AsyncWrite and AsyncClose must be called on the connection's event
// loop thread, that is from a Handler callback.
type Conn struct {
	c       *conn
	ctx     interface{}
	pending []byte // Framed: incomplete frame
}

// Write queues b for sending and returns len(b). Data is sent in order. Unless b lies
// inside the data passed to the current OnTraffic call, it is copied, so b can be reused
// once Write returns. Data inside the received buffer is sent in place: do not modify it
// afterwards. Everything written during one callback is sent together after it returns.
func (cn *Conn) Write(b []byte) (int, error) {
	c := cn.c
	if c.closed || c.closeAfter {
		return 0, ErrConnClosed
	}
	c.send(b)
	return len(b), nil
}

// WriteNoCopy queues b without copying it. release (may be nil) is called once the kernel
// no longer uses b; b must not be modified before that. With zero-copy send enabled
// (WithZeroCopy) and b large enough, b is sent with SEND_ZC. If the connection is closed,
// release is called at once and ErrConnClosed is returned.
func (cn *Conn) WriteNoCopy(b []byte, release func()) error {
	if cn.c.closeAfter {
		if release != nil {
			release()
		}
		return ErrConnClosed
	}
	return cn.c.writeNoCopy(b, release)
}

// Close closes the connection after the data already written has been sent (bounded by
// the write timeout, if set). Later writes fail with ErrConnClosed. OnClose follows.
func (cn *Conn) Close() error {
	c := cn.c
	if c.closed {
		return ErrConnClosed
	}
	c.closeGracefully()
	return nil
}

// AsyncWrite copies b and writes it from the connection's event loop. It is safe to call
// from any goroutine. done (may be nil) is called on the event loop once the data is
// queued, with ErrConnClosed if the connection was closed by then; it must not block.
func (cn *Conn) AsyncWrite(b []byte, done func(error)) error {
	buf := append([]byte(nil), b...)
	c := cn.c
	return c.rn.Post(func() {
		_, err := cn.Write(buf)
		if err == nil {
			c.checkBackpressure()
		}
		if done != nil {
			done(err)
		}
	})
}

// AsyncClose closes the connection from its event loop, like Close. It is safe to call
// from any goroutine.
func (cn *Conn) AsyncClose() error {
	return cn.c.rn.Post(func() { cn.Close() })
}

// TLSConnectionState returns the TLS state of a TLS connection (WithTLS, WithDialTLS); ok is false
// for plain connections.
func (cn *Conn) TLSConnectionState() (state tls.ConnectionState, ok bool) {
	if cn.c.tls == nil || !cn.c.tls.ready {
		return tls.ConnectionState{}, false
	}
	return cn.c.tls.tc.ConnectionState(), true
}

// Context returns the value set with SetContext.
func (cn *Conn) Context() interface{} { return cn.ctx }

// SetContext stores per-connection state.
func (cn *Conn) SetContext(ctx interface{}) { cn.ctx = ctx }

// Fd returns the socket file descriptor. Do not close it or change its blocking mode.
func (cn *Conn) Fd() int { return cn.c.fd }

// Buffered returns the number of bytes queued and not yet sent.
func (cn *Conn) Buffered() int { return cn.c.qBytes }

// RemoteAddr returns the peer address, or nil if it is not available.
func (cn *Conn) RemoteAddr() net.Addr {
	sa, err := unix.Getpeername(cn.c.fd)
	if err != nil {
		return nil
	}
	return sockaddrToNetAddr(sa)
}

// LocalAddr returns the local address, or nil if it is not available.
func (cn *Conn) LocalAddr() net.Addr {
	sa, err := unix.Getsockname(cn.c.fd)
	if err != nil {
		return nil
	}
	return sockaddrToNetAddr(sa)
}

// apiHandler connects a Handler to the connection state machine. OnBoot/OnShutdown are called once by the Server.
type apiHandler struct {
	h Handler
	s *Server
}

func (a apiHandler) boot(*URingNet)     {}
func (a apiHandler) shutdown(*URingNet) {}

func (a apiHandler) open(c *conn) { c.apiAction(a.h.OnOpen(&c.api), a.s) }

func (a apiHandler) traffic(c *conn, data []byte) { c.apiAction(a.h.OnTraffic(&c.api, data), a.s) }

func (a apiHandler) written(*conn) {}

func (a apiHandler) closed(c *conn) { a.h.OnClose(&c.api, c.err) }

// apiAction handles the action returned by a Handler callback. Data is sent with Conn.Write; the old write actions count as None here.
func (c *conn) apiAction(action Action, s *Server) {
	switch action {
	case Close, EchoAndClose:
		c.closeGracefully()
	case Shutdown:
		go s.Close()
	}
}
