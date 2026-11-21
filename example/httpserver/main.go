package main

import (
	"bytes"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/y001j/uringnet"
	socket "github.com/y001j/uringnet/sockets"
)

// 一个最简 HTTP/1.1 服务，只用于压测：每个以 \r\n\r\n 结尾的请求回复一个固定响应。
type httpServer struct {
	uringnet.BuiltinEventEngine
}

// httpCodec 保存每个连接未处理完的请求数据（请求可能跨多次 recv）
type httpCodec struct {
	buf []byte
}

var delimiter = []byte("\r\n\r\n")

func appendResponse(b []byte) []byte {
	b = append(b, "HTTP/1.1 200 OK\r\nServer: uringNet\r\nContent-Type: text/plain\r\nDate: "...)
	b = time.Now().UTC().AppendFormat(b, "Mon, 02 Jan 2006 15:04:05 GMT")
	return append(b, "\r\nContent-Length: 12\r\n\r\nHello World!"...)
}

func (hs *httpServer) OnOpen(data *uringnet.UserData) ([]byte, uringnet.Action) {
	data.Context = &httpCodec{}
	return nil, uringnet.None
}

func (hs *httpServer) OnTraffic(data *uringnet.UserData, ringnet *uringnet.URingNet) uringnet.Action {
	codec := data.Context.(*httpCodec)
	in := data.Buffer
	if len(codec.buf) > 0 {
		codec.buf = append(codec.buf, in...)
		in = codec.buf
	}
	for {
		i := bytes.Index(in, delimiter)
		if i < 0 {
			break
		}
		data.WriteBuf = appendResponse(data.WriteBuf)
		in = in[i+len(delimiter):]
	}
	if len(in) > 64*1024 {
		return uringnet.Close // 请求头过大
	}
	// 保留未处理完的部分（data.Buffer 在回调后失效，必须拷贝）
	codec.buf = append(codec.buf[:0], in...)
	if len(data.WriteBuf) == 0 {
		return uringnet.None
	}
	return uringnet.Write
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: httpserver <address>")
		os.Exit(1)
	}
	addr := os.Args[1]
	rings := runtime.NumCPU()

	options := socket.SocketOptions{TCPNoDelay: socket.TCPNoDelay, ReusePort: true}
	ringNets, err := uringnet.NewMany(uringnet.NetAddress{AddrType: socket.Tcp4, Address: addr}, 4096, false, rings, options, &httpServer{})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	// 可选：rn.SpinTime（阻塞前忙轮询，降低延迟）、rn.ZeroCopyThreshold（大块数据零拷贝发送）
	// 每个 ring 256 个接收缓冲区：ring 是 FIFO，缓冲区越多拷贝时缓存越冷，够用即可
	loop, err := uringnet.SetLoops(ringNets, 256)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	loop.RunMany()
	fmt.Printf("HTTP server started on %s with %d rings\n", addr, rings)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; loop.Shutdown() }()
	loop.Wait()
}
