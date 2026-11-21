//go:build linux

// uringnet echo 服务（压测用），可通过参数调整缓冲区、轮询与零拷贝
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/y001j/uringnet"
	socket "github.com/y001j/uringnet/sockets"
)

type echo struct{ uringnet.BuiltinEventEngine }

func (e *echo) OnTraffic(d *uringnet.UserData, _ *uringnet.URingNet) uringnet.Action {
	d.WriteBuf = d.Buffer
	return uringnet.Echo
}

func main() {
	rings := flag.Int("rings", runtime.NumCPU(), "number of event loops")
	bufs := flag.Int("bufs", 256, "receive buffers per ring (power of 2)")
	bufSize := flag.Int("bufsize", 16384, "receive buffer size")
	spin := flag.Duration("spin", 0, "busy-poll time before blocking, e.g. 50us")
	zc := flag.Int("zc", 0, "zero-copy send threshold in bytes, 0 disables")
	bundle := flag.String("bundle", "auto", "recv bundles: auto (Linux 6.16+), on (force; 6.10-6.15 kernels may corrupt data), off")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Println("usage: uringnet [flags] <addr>")
		os.Exit(1)
	}
	opts := socket.SocketOptions{TCPNoDelay: socket.TCPNoDelay, ReusePort: true}
	rns, err := uringnet.NewMany(uringnet.NetAddress{AddrType: socket.Tcp4, Address: flag.Arg(0)}, 4096, false, *rings, opts, &echo{})
	if err != nil {
		panic(err)
	}
	for _, rn := range rns {
		rn.BufferSize = *bufSize
		rn.SpinTime = *spin
		rn.ZeroCopyThreshold = *zc
		switch *bundle {
		case "on":
			rn.RecvBundle = uringnet.BundleOn
		case "off":
			rn.RecvBundle = uringnet.BundleOff
		}
	}
	l, err := uringnet.SetLoops(rns, *bufs)
	if err != nil {
		panic(err)
	}
	l.RunMany()
	l.Wait()
}
