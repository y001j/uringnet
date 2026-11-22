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

// 一个最简 HTTP/1.1 服务，只用于压测：每个以 \r\n\r\n 结尾的请求回复一个固定响应。
// 连接管理遵循 HTTP 规则：HTTP/1.1 默认长连接，带 "Connection: close" 时回复后关闭；
// HTTP/1.0 默认回复后关闭，带 "Connection: keep-alive" 时保持（ab 不加 -k 时依赖服务端关闭连接）。
type httpServer struct {
	uringnet.BuiltinHandler
}

// httpCodec 保存每个连接未处理完的请求数据（请求可能跨多次 recv）和复用的响应缓冲区
type httpCodec struct {
	buf []byte
	out []byte
}

var delimiter = []byte("\r\n\r\n")

type connMode int

const (
	modeDefault   connMode = iota // HTTP/1.1 长连接，响应里不需要 Connection 头
	modeKeepAlive                 // HTTP/1.0 + keep-alive：响应里必须回 Connection: keep-alive
	modeClose                     // 回复后关闭连接
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
	connToken = []byte("onnection:") // 不区分 Connection/connection 的首字母
)

// requestMode 根据请求行的协议版本和 Connection 头决定连接如何处理。
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
		closing = mode == modeClose // 回复后关闭，忽略之后的数据
		in = in[i+len(delimiter):]
	}
	if len(out) > 0 {
		c.Write(out) // 拷贝进发送队列，out 可以复用
	}
	codec.out = out
	if closing {
		return uringnet.Close // 已写入的响应发完后关闭
	}
	if len(in) > 64*1024 {
		return uringnet.Close // 请求头过大
	}
	// 保留未处理完的部分（data 在回调后失效，必须拷贝）
	codec.buf = append(codec.buf[:0], in...)
	return uringnet.None
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: httpserver <address>")
		os.Exit(1)
	}
	srv, err := uringnet.Listen("tcp", os.Args[1], &httpServer{},
		// 每个 ring 256 个接收缓冲区：ring 是 FIFO，缓冲区越多拷贝时缓存越冷，够用即可
		uringnet.WithBuffers(256, 16<<10),
		// 可选：uringnet.WithBusyPoll(50*time.Microsecond) 降低延迟
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
