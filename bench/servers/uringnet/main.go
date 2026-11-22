//go:build linux

// uringnet echo server for benchmarks; flags tune buffers, polling, zero copy and the backend.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/y001j/uringnet"
)

type echo struct{ uringnet.BuiltinHandler }

// OnTraffic writes data back as is: it lies in the receive buffer, so it is sent in place.
func (echo) OnTraffic(c *uringnet.Conn, data []byte) uringnet.Action {
	c.Write(data)
	return uringnet.None
}

func main() {
	rings := flag.Int("rings", runtime.NumCPU(), "number of event loops")
	bufs := flag.Int("bufs", 256, "receive buffers per ring (power of 2)")
	bufSize := flag.Int("bufsize", 16384, "receive buffer size")
	spin := flag.Duration("spin", 0, "busy-poll time before blocking, e.g. 50us (adaptive)")
	spinFixed := flag.Bool("spinfixed", false, "always busy-poll, even when the loop is busy")
	spinBusy := flag.Int("spinbusy", 0, "adaptive polling: stop at this average batch size (0 = default 128)")
	spinIdle := flag.Int("spinidle", 0, "adaptive polling: resume below this average batch size (0 = default 64)")
	zc := flag.Int("zc", 0, "zero-copy send threshold in bytes, 0 disables")
	inc := flag.Bool("inc", false, "incremental buffer ring (Linux 6.12+, experimental)")
	backend := flag.String("backend", "auto", "event loop backend: auto, io_uring or epoll")
	sq := flag.Uint("sq", 512, "submission queue size per ring (completion queue is 4x)")
	bundle := flag.String("bundle", "auto", "recv bundles: auto (Linux 6.16+), on (force; 6.10-6.15 kernels may corrupt data), off")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Println("usage: uringnet [flags] <addr>")
		os.Exit(1)
	}
	opts := []uringnet.Option{
		uringnet.WithRings(*rings),
		uringnet.WithBuffers(*bufs, *bufSize),
		uringnet.WithZeroCopy(*zc),
		uringnet.WithBusyPollThresholds(*spinBusy, *spinIdle),
		uringnet.WithSQSize(*sq),
	}
	switch *backend {
	case "io_uring":
		opts = append(opts, uringnet.WithBackend(uringnet.BackendIOUring))
	case "epoll":
		opts = append(opts, uringnet.WithBackend(uringnet.BackendEpoll))
	}
	if *spinFixed {
		opts = append(opts, uringnet.WithBusyPollFixed(*spin))
	} else {
		opts = append(opts, uringnet.WithBusyPoll(*spin))
	}
	if *inc {
		opts = append(opts, uringnet.WithIncrementalBuffers())
	}
	switch *bundle {
	case "on":
		opts = append(opts, uringnet.WithRecvBundle(uringnet.BundleOn))
	case "off":
		opts = append(opts, uringnet.WithRecvBundle(uringnet.BundleOff))
	}
	srv, err := uringnet.Listen("tcp4", flag.Arg(0), echo{}, opts...)
	if err != nil {
		panic(err)
	}
	srv.Serve()
}
