package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/y001j/uringnet"
	socket "github.com/y001j/uringnet/sockets"
)

type echoServer struct {
	uringnet.BuiltinEventEngine
}

// OnTraffic 在每次收到数据时调用；data.Buffer 只在回调期间有效，WriteBuf 会被框架拷贝
func (es *echoServer) OnTraffic(data *uringnet.UserData, ringnet *uringnet.URingNet) uringnet.Action {
	data.WriteBuf = data.Buffer
	return uringnet.Echo
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: echoserver <address>")
		fmt.Println("Example: echoserver :8080")
		os.Exit(1)
	}
	addr := os.Args[1]
	rings := runtime.NumCPU()

	options := socket.SocketOptions{TCPNoDelay: socket.TCPNoDelay, ReusePort: true}
	ringNets, err := uringnet.NewMany(uringnet.NetAddress{AddrType: socket.Tcp4, Address: addr}, 4096, false, rings, options, &echoServer{})
	if err != nil {
		fmt.Printf("Failed to create uring instances: %v\n", err)
		os.Exit(1)
	}

	// 可选：rn.SpinTime（阻塞前忙轮询，降低延迟）、rn.ZeroCopyThreshold（大块数据零拷贝发送）
	// 每个 ring 256 个接收缓冲区：ring 是 FIFO，缓冲区越多拷贝时缓存越冷，够用即可
	loop, err := uringnet.SetLoops(ringNets, 256)
	if err != nil {
		fmt.Printf("Failed to set loops: %v\n", err)
		os.Exit(1)
	}

	loop.RunMany()
	fmt.Printf("Echo server started on %s with %d rings\n", addr, rings)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; loop.Shutdown() }()
	loop.Wait()
}
