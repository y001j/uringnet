package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/y001j/uringnet"
)

type echoServer struct {
	uringnet.BuiltinHandler
}

// OnTraffic 在每次收到数据时调用。data 只在回调期间有效；它位于接收缓冲区中，
// 原样写回时直接从接收缓冲区发送，不拷贝。
func (echoServer) OnTraffic(c *uringnet.Conn, data []byte) uringnet.Action {
	c.Write(data)
	return uringnet.None
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: echoserver <address>")
		fmt.Println("Example: echoserver :8080")
		os.Exit(1)
	}

	srv, err := uringnet.Listen("tcp", os.Args[1], echoServer{},
		// 每个 ring 256 个 16KB 接收缓冲区：ring 是 FIFO，缓冲区越多拷贝时缓存越冷，够用即可
		uringnet.WithBuffers(256, 16<<10),
		uringnet.WithIdleTimeout(5*time.Minute),
		uringnet.WithLogger(slog.Default()),
		// 可选：uringnet.WithBusyPoll(50*time.Microsecond) 降低延迟，uringnet.WithZeroCopy(16<<10) 大块数据零拷贝发送
	)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Printf("Echo server listening on %s\n", srv.Addr())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx) // 不再接受新连接，已写入的数据发完后关闭各个连接
	}()
	if err := srv.Serve(); !errors.Is(err, uringnet.ErrServerClosed) {
		fmt.Println(err)
		os.Exit(1)
	}
}
