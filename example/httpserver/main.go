package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/y001j/uringnet"
)

// A minimal HTTP/1.1 server for benchmarks: every request ending in \r\n\r\n gets a fixed response.
// Connections follow HTTP rules: HTTP/1.1 is persistent unless "Connection: close", which closes after the
// response; HTTP/1.0 closes after the response unless "Connection: keep-alive" (ab without -k relies on that).
type httpServer struct {
	uringnet.BuiltinHandler
}

// httpCodec keeps a connection's unprocessed request bytes (requests can span receives) and a reusable response buffer.
type httpCodec struct {
	buf []byte
	out []byte
}

var delimiter = []byte("\r\n\r\n")

type connMode int

const (
	modeDefault   connMode = iota // HTTP/1.1 persistent: no Connection header needed in the response
	modeKeepAlive                 // HTTP/1.0 + keep-alive: the response must say Connection: keep-alive
	modeClose                     // close after the response
)

func appendResponse(b []byte, mode connMode) []byte {
	b = append(b, "HTTP/1.1 200 OK\r\nServer: uringNet\r\nContent-Type: text/plain\r\nDate: "...)
	b = time.Now().UTC().AppendFormat(b, "Mon, 02 Jan 2006 15:04:05 GMT")
	switch mode {
	case modeKeepAlive:
		b = append(b, "\r\nConnection: keep-alive"...)
	case modeClose:
		b = append(b, "\r\nConnection: close"...)
	}
	return append(b, "\r\nContent-Length: 12\r\n\r\nHello World!"...)
}

var (
	http10    = []byte("HTTP/1.0")
	connToken = []byte("onnection:") // matches Connection and connection
)

// requestMode decides how to handle the connection from the request line's version and the Connection header.
func requestMode(header []byte) connMode {
	line := header
	if i := bytes.IndexByte(header, '\r'); i >= 0 {
		line = header[:i]
	}
	v10 := bytes.HasSuffix(line, http10)
	var value []byte
	if i := bytes.Index(header, connToken); i >= 0 {
		value = header[i+len(connToken):]
		if j := bytes.IndexByte(value, '\r'); j >= 0 {
			value = value[:j]
		}
		value = bytes.ToLower(bytes.TrimSpace(value))
	}
	switch {
	case bytes.Contains(value, []byte("close")):
		return modeClose
	case v10 && bytes.Contains(value, []byte("keep-alive")):
		return modeKeepAlive
	case v10:
		return modeClose
	}
	return modeDefault
}

func (hs *httpServer) OnOpen(c *uringnet.Conn) uringnet.Action {
	c.SetContext(&httpCodec{})
	return uringnet.None
}

func (hs *httpServer) OnTraffic(c *uringnet.Conn, data []byte) uringnet.Action {
	codec := c.Context().(*httpCodec)
	in := data
	if len(codec.buf) > 0 {
		codec.buf = append(codec.buf, in...)
		in = codec.buf
	}
	out := codec.out[:0]
	closing := false
	for !closing {
		i := bytes.Index(in, delimiter)
		if i < 0 {
			break
		}
		mode := requestMode(in[:i])
		out = appendResponse(out, mode)
		closing = mode == modeClose // close after this response and ignore anything after it
		in = in[i+len(delimiter):]
	}
	if len(out) > 0 {
		c.Write(out) // copied into the send queue, so out can be reused
	}
	codec.out = out
	if closing {
		return uringnet.Close // close once the responses written are sent
	}
	if len(in) > 64*1024 {
		return uringnet.Close // request header too large
	}
	// Keep the unprocessed part (data is invalid after the callback, so it must be copied)
	codec.buf = append(codec.buf[:0], in...)
	return uringnet.None
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: httpserver <address>")
		os.Exit(1)
	}
	srv, err := uringnet.Listen("tcp", os.Args[1], &httpServer{},
		// 256 receive buffers per ring: they are used in FIFO order, so more buffers mean colder caches; enough is best
		uringnet.WithBuffers(256, 16<<10),
		// Optional: uringnet.WithBusyPoll(50*time.Microsecond) for lower latency
	)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Printf("HTTP server listening on %s\n", srv.Addr())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	if err := srv.Serve(); !errors.Is(err, uringnet.ErrServerClosed) {
		fmt.Println(err)
		os.Exit(1)
	}
}
