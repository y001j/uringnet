# UringNet

[![Go Report Card](https://goreportcard.com/badge/github.com/y001j/UringNet)](https://goreportcard.com/report/github.com/y001j/UringNet)
![https://codecov.io/gh/jceb/vim-orgmode](https://codecov.io/gh/y001j/uringnet/branch/master/graph/badge.svg)
[![Build](https://github.com/y001j/uringnet/actions/workflows/go.yml/badge.svg?branch=master)](https://github.com/y001j/uringnet/actions/workflows/go.yml)
[![GoDoc](https://godoc.org/github.com/y001j/uringnet?status.svg)](https://pkg.go.dev/github.com/y001j/uringnet)

![logo](https://pic.maienzx.com/qiniuPic/202310031719836.png)

## Introduction

### Why I created this 

UringNet is a high performance network I/O framework. It is light weighted and developed by Golang. The foundation of UringNet is [io_uring](https://kernel.dk/io_uring.pdf) - a new async IO interface - from Linux Kernel since version 5.1 in 2019. This project just comes from experimental projects in my edge computing and IoT research. I am just want to find a way to build a simple but high performance network package transmit tool in IoT gateways. I have tried traditional select/epoll and then tried io_uring, finally , the io_uring get much better performance. In most cases, the performance improvement ranges from **10% to 30%**, while in the most extreme case, the performance improvement can reach up to 66%.

### Should You adopt it in your project

Though UringNet is designed to be used in IoT platforms originally, it does provide basic TCP and UDP network functions. If your project really need to handle extremely huge data traffic, you can give it a try. 

In most cases, you could just use more mature network frameworks, for example, Go [net](https://golang.org/pkg/net/)/[http](https://pkg.go.dev/net/http), [netty](https://netty.io/), [libuv](https://github.com/libuv/libuv).

## Get Started

UringNet extensively references existing network frameworks such as gnet and Netty, with some usage patterns closely resembling those of gnet. Please note that UringNet is based on io_uring and, therefore, requires running on the Linux operating system with a kernel version **6.0 or later** (multishot recv / provided buffer ring; 6.1+ additionally enables `SINGLE_ISSUER | DEFER_TASKRUN`).

Since this version the event loop is built on the `Loop` API of [iouring-go](https://github.com/y001j/iouring-go): one io_uring and one locked OS thread per ring, multishot accept/recv, a provided buffer ring, and a single `io_uring_enter` per loop iteration.

### Install UringNet:

```shell
go get -u github.com/y001j/uringnet
```

### echo server example

```go
package main

import (
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

// OnTraffic runs on the ring's event-loop thread every time data arrives.
// data.Buffer is only valid during the callback; WriteBuf is copied (or sent
// zero-copy when it points into data.Buffer).
func (es *echoServer) OnTraffic(data *uringnet.UserData, ringnet *uringnet.URingNet) uringnet.Action {
	data.WriteBuf = data.Buffer
	return uringnet.Echo
}

func main() {
	options := socket.SocketOptions{TCPNoDelay: socket.TCPNoDelay, ReusePort: true}
	ringNets, err := uringnet.NewMany(uringnet.NetAddress{AddrType: socket.Tcp4, Address: os.Args[1]},
		4096, false, runtime.NumCPU(), options, &echoServer{})
	if err != nil {
		panic(err)
	}
	loop, err := uringnet.SetLoops(ringNets, 256) // 256 receive buffers per ring
	if err != nil {
		panic(err)
	}
	loop.RunMany()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; loop.Shutdown() }()
	loop.Wait()
}
```

### Important APIs

| API | Description |
|---|---|
| `NewMany(addr, size, sqpoll, num, options, handler)` | create `num` rings; with `ReusePort` each ring gets its own listening socket. `sqpoll` is deprecated and ignored. Only stream protocols (tcp/tcp4/tcp6/unix) are supported. |
| `URingNet.BufferSize` | size of one receive buffer, set before `SetLoops` (default 16KB) |
| `URingNet.SpinTime` | busy-poll before blocking (e.g. `50*time.Microsecond`): lower latency at the cost of CPU |
| `URingNet.ZeroCopyThreshold` | enable zero-copy send (`IORING_OP_SEND_ZC`) for writes of at least this many bytes; 0 disables |
| `SetLoops(rings, bufferCount)` | create the event loops; `bufferCount` is rounded up to a power of 2 (max 32768). The ring is FIFO, so more buffers means colder caches: use as few as the load needs (256 is a good start) |
| `UserData.WriteNoCopy(b, release)` | queue `b` without copying; `release` is called once the kernel no longer uses it |
| `Ringloop.RunMany()` / `Wait()` / `Shutdown()` | start accepting / block until shut down / close all connections and loops |
| `URingNet.Post(fn)` | run `fn` on the ring's event-loop thread (from any goroutine) |

Callback semantics:

- All callbacks except `OnTick` run on the ring's event-loop thread and must not block.
- `UserData` is per connection and lives for the whole connection; use `UserData.Context` for per-connection state.
- Actions: `Echo`/`Write` send `WriteBuf`; `EchoAndClose` sends then closes; `Close` closes; `Shutdown` shuts down the engine; `None`/`Read` do nothing (receiving is always active).
- Writes on one connection are ordered; writes issued while a send is in flight are coalesced; short writes are handled.
- `OnClose` is called exactly once per connection, including peer close, errors and `Shutdown`.

### Zero-copy send

With `ZeroCopyThreshold > 0` the receive buffer ring is also registered as an io_uring *fixed buffer*
(`IORING_REGISTER_BUFFERS`). That one memory region is shared by user space and the kernel:

```
          kernel receives into it            kernel transmits from it
 NIC/TCP ───────────────────────►  buffer ring (pinned, mapped once)  ───────────────────► NIC/TCP
          (provided buffers)          ▲ OnTraffic sees data.Buffer       (SEND_ZC + FIXED_BUF)
                                      │ in place, no copy
```

- Echo/forwarding (`data.WriteBuf = data.Buffer`): the data is sent from the receive buffer itself with
  `SendZCFixed`; the buffer goes back to the ring only after the kernel's notification CQE (`IORING_CQE_F_NOTIF`).
- Large responses you own (cached files, pre-built payloads): `data.WriteNoCopy(b, release)` sends `b` with `SEND_ZC`
  without copying it into the socket buffer.
- What stays: the receive side still copies from the socket into the buffer ring. Removing that needs io_uring
  zero-copy receive (zcrx, `IORING_OP_RECV_ZC`, Linux 6.15+), which requires NIC support (header split, flow steering,
  a dedicated RX queue) and is not implemented.
- Caveats: it only pays off above roughly 10KB per send; **traffic to a local socket (loopback, containers on the same host)
  is always copied by the kernel**, so it is slower there; pinned pages count against `RLIMIT_MEMLOCK`. If registration
  fails, uringnet falls back to plain `SEND_ZC`. If `SEND_ZC` fails with `ENOMEM`/`EOPNOTSUPP`, it falls back to normal sends.

### Benchmarks

`bench/` has an echo benchmark comparing Go `net`, hand-written epoll, gnet, raw iouring-go and uringnet, runnable in Docker:
see [bench/README.md](bench/README.md).

### Testing on macOS

```shell
docker run --rm --security-opt seccomp=unconfined -v "$PWD":/src -w /src golang:1.22 go test -race .
```

`seccomp=unconfined` is required because Docker's default seccomp profile blocks io_uring.

## Benchmark

### Echo Stress Testing

In echo stress testing, the benchmark compared [go net](https://golang.org/pkg/net/), [gnet](https://github.com/panjf2000/gnet) and UringNet

Echo test tool: [rust echo bench](https://github.com/haraldh/rust_echo_bench)

> **Test environment:**
>
> Host:
>
> CPU：Intel Core i5 12400F 6 core 12 thread
>
> Memory: DDR4 32GB
>
> OS：Windows 11
>
> Virtual Machine:
>
> VMware® Workstation 16 Pro
>
> Processors: 8P ; Memory 16GB; Intel VT-x
>
> OS：Ubuntu 22.04.1 LTS， Kernel：5.19.3

#### Test Result

![echo](https://pic.maienzx.com/qiniuPic/202211061218743.png)

### HTTP Stress Testing

In HTTP stress testing, the benchmark compared [gnet](https://github.com/panjf2000/gnet), fasthttp and UringNet

Echo test tool: [wrk](https://github.com/wg/wrk)

> Test Enviroment 
>
> Host：
>
> CPU：Intel Core i5 12400F 6 core 12 thread
>
> Memory: DDR4 32GB
>
> OS：Windows 11
>
> Testing Virtual Machine：
>
> VMware® Workstation 16 Pro
>
> Processors: 8P ; Memory 16GB; Intel VT-x
>
> OS：Ubuntu 22.04.1 LTS， Kernel：5.19.3
>
> wrk Virtual Machine：
>
> VMware® Workstation 16 Pro
>
> Processors: 4P ; Memory 8GB; Intel VT-x
>
> OS：Ubuntu 20.04.5 LTS， Kernel：5.15.0   

#### Test Result

![image-20221212113551004](https://pic.maienzx.com/qiniuPic/image-20221212113551004.png)

## Known Issues

> **Warning:** The project is still in development, there are still many bugs and performance issues. If you find any bugs or have any suggestions, please feel free to open an issue.

During our performance tests with wrk, we observed that UringNet requires a warm-up period during HTTP stress testing. Typically, a warm-up time of around 5 minutes is necessary to ensure that the UringNet test code reaches its peak performance, which surpasses the current fastest framework by more than 10%. I am still investigating the root cause of this behavior.
