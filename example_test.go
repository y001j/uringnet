//go:build linux
// +build linux

package uringnet_test

import (
	"context"
	"errors"
	"expvar"
	"log"
	"log/slog"
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
