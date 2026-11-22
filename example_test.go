//go:build linux
// +build linux

package uringnet_test

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"expvar"
	"log"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/y001j/uringnet"
)

type echo struct{ uringnet.BuiltinHandler }

func (echo) OnTraffic(c *uringnet.Conn, data []byte) uringnet.Action {
	c.Write(data) // data lies in the receive buffer: sent in place, without a copy
	return uringnet.None
}

// An echo server that shuts down gracefully on SIGINT or SIGTERM.
func ExampleListen() {
	srv, err := uringnet.Listen("tcp", ":8080", echo{},
		uringnet.WithBuffers(256, 16<<10),
		uringnet.WithIdleTimeout(5*time.Minute),
		uringnet.WithLogger(slog.Default()),
	)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	if err := srv.Serve(); !errors.Is(err, uringnet.ErrServerClosed) {
		log.Fatal(err)
	}
}

// pusher keeps the open connections and sends to them from other goroutines.
type pusher struct {
	uringnet.BuiltinHandler
	conns chan *uringnet.Conn
}

func (p *pusher) OnOpen(c *uringnet.Conn) uringnet.Action {
	p.conns <- c
	return uringnet.None
}

// Writing to a connection from another goroutine with AsyncWrite.
func ExampleConn_AsyncWrite() {
	p := &pusher{conns: make(chan *uringnet.Conn, 16)}
	srv, err := uringnet.Listen("tcp", ":8080", p)
	if err != nil {
		log.Fatal(err)
	}
	go srv.Serve()

	for c := range p.conns {
		go func(c *uringnet.Conn) {
			for t := range time.Tick(time.Second) {
				err := c.AsyncWrite([]byte(t.Format(time.RFC3339)+"\n"), nil)
				if err != nil { // the server is closed
					return
				}
			}
		}(c)
	}
}

// Per-connection state, and the reason a connection was closed.
type session struct {
	uringnet.BuiltinHandler
}

type state struct{ requests int }

func (session) OnOpen(c *uringnet.Conn) uringnet.Action {
	c.SetContext(&state{})
	return uringnet.None
}

func (session) OnTraffic(c *uringnet.Conn, data []byte) uringnet.Action {
	st := c.Context().(*state)
	st.requests++
	if st.requests > 100 {
		return uringnet.Close // after the data already written is sent
	}
	c.Write(data)
	return uringnet.None
}

func (session) OnClose(c *uringnet.Conn, err error) {
	switch {
	case err == nil: // closed by us
	case errors.Is(err, uringnet.ErrIdleTimeout):
		log.Printf("%v: idle", c.RemoteAddr())
	default:
		log.Printf("%v: %v", c.RemoteAddr(), err)
	}
}

func ExampleHandler() {
	srv, err := uringnet.Listen("tcp", ":8080", session{}, uringnet.WithIdleTimeout(time.Minute))
	if err != nil {
		log.Fatal(err)
	}
	srv.Serve()
}

// Publishing the server's counters with expvar (served at /debug/vars by net/http).
func ExampleServer_Stats() {
	srv, err := uringnet.Listen("tcp", ":8080", echo{})
	if err != nil {
		log.Fatal(err)
	}
	expvar.Publish("uringnet", expvar.Func(func() any { return srv.Stats() }))
	srv.Serve()
}

// frameEcho answers every length-prefixed frame with the same frame.
type frameEcho struct{ uringnet.BuiltinHandler }

func (frameEcho) OnMessage(c *uringnet.Conn, msg []byte) uringnet.Action {
	c.Write(msg) // a frame that arrived in one piece is sent back in place
	return uringnet.None
}

// A server for frames with a 2-byte big-endian length header, payload up to 64KB.
// Partial frames are buffered per connection; OnMessage sees whole frames only.
func ExampleFramed() {
	framer := uringnet.NewLengthField(2, binary.BigEndian, 64<<10)
	srv, err := uringnet.Listen("tcp", ":9000", uringnet.Framed(framer, frameEcho{}))
	if err != nil {
		log.Fatal(err)
	}
	srv.Serve()
}

// tcpProxy forwards every accepted connection to an upstream server. Conn.DialAsync puts
// the upstream connection on the same event loop, so the two can write to each other
// directly from their callbacks.
type tcpProxy struct {
	uringnet.BuiltinHandler
	upstream string
}

type side struct {
	peer    *uringnet.Conn
	pending []byte // data from the client before the upstream connection is up
}

func (p *tcpProxy) OnOpen(c *uringnet.Conn) uringnet.Action {
	if c.Context() != nil {
		return uringnet.None // the upstream connection: its context is set before OnOpen
	}
	down := &side{}
	c.SetContext(down)
	c.DialAsync("tcp", p.upstream, &side{peer: c}, func(up *uringnet.Conn, err error) {
		if err != nil {
			c.Close()
			return
		}
		down.peer = up
		up.Write(down.pending)
		down.pending = nil
	})
	return uringnet.None
}

func (p *tcpProxy) OnTraffic(c *uringnet.Conn, data []byte) uringnet.Action {
	s := c.Context().(*side)
	if s.peer == nil {
		s.pending = append(s.pending, data...)
	} else {
		s.peer.Write(data)
	}
	return uringnet.None
}

func (p *tcpProxy) OnClose(c *uringnet.Conn, err error) {
	if s, ok := c.Context().(*side); ok && s.peer != nil {
		s.peer.Close()
	}
}

// A TCP proxy from :8080 to 127.0.0.1:9000.
func ExampleConn_DialAsync() {
	srv, err := uringnet.Listen("tcp", ":8080", &tcpProxy{upstream: "127.0.0.1:9000"})
	if err != nil {
		log.Fatal(err)
	}
	srv.Serve()
}

// A client: connections are dialed and then handled with the same callbacks as a server's.
func ExampleNewClient() {
	cl, err := uringnet.NewClient(echo{}, uringnet.WithRings(1))
	if err != nil {
		log.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := cl.Dial(ctx, "tcp", "127.0.0.1:9000", nil)
	if err != nil {
		log.Fatal(err)
	}
	c.AsyncWrite([]byte("hello"), nil)
}

// A UDP echo server. The source address is a netip.AddrPort, so receiving allocates nothing.
func ExampleListenPacket() {
	srv, err := uringnet.ListenPacket("udp", ":5683",
		uringnet.PacketHandlerFunc(func(pc *uringnet.PacketConn, data []byte, from netip.AddrPort) {
			pc.WriteTo(data, from)
		}),
		uringnet.WithBuffers(256, 2048)) // datagrams up to about 2000 bytes
	if err != nil {
		log.Fatal(err)
	}
	srv.Serve()
}

// A TLS echo server. Handlers see plaintext; OnOpen runs after the handshake.
func ExampleWithTLS() {
	cert, err := tls.LoadX509KeyPair("server.crt", "server.key")
	if err != nil {
		log.Fatal(err)
	}
	srv, err := uringnet.Listen("tcp", ":8443", echo{},
		uringnet.WithTLS(&tls.Config{Certificates: []tls.Certificate{cert}}))
	if err != nil {
		log.Fatal(err)
	}
	srv.Serve()
}
