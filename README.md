# UringNet

[![Go Report Card](https://goreportcard.com/badge/github.com/y001j/UringNet)](https://goreportcard.com/report/github.com/y001j/UringNet)
![https://codecov.io/gh/jceb/vim-orgmode](https://codecov.io/gh/y001j/uringnet/branch/master/graph/badge.svg)
[![Build](https://github.com/y001j/uringnet/actions/workflows/go.yml/badge.svg?branch=master)](https://github.com/y001j/uringnet/actions/workflows/go.yml)
[![GoDoc](https://godoc.org/github.com/y001j/uringnet?status.svg)](https://pkg.go.dev/github.com/y001j/uringnet)

![logo](https://pic.maienzx.com/qiniuPic/202310031719836.png)

## Introduction

### Why I created this 

UringNet is a lightweight network I/O framework written in Go, built on [io_uring](https://kernel.dk/io_uring.pdf), the asynchronous I/O interface Linux added in 5.1 (2019). It comes from experimental projects in my edge computing and IoT research: I wanted a simple but fast way to move network packets in IoT gateways, tried select/epoll first, and then io_uring.

Measured against a hand-written epoll loop and [gnet](https://github.com/panjf2000/gnet) (see [Benchmark](#benchmark), Linux 6.17):

- **Busy server** (CPU-bound): about the same as epoll and gnet, from equal to roughly 10% faster.
- **Low and medium load, latency-sensitive** (request/response): with `SpinTime` enabled, 1.1–1.5x the throughput of epoll and
  up to half the median latency, at the cost of extra CPU. Without it, up to ~20% behind epoll.
- **Bulk pipelined transfers**: 1.4–1.6x of epoll, but behind gnet (1.7–1.8x).

What io_uring brings is far fewer system calls (under load as few as 0.04 per request, versus 2–3 for epoll), batched submission, and
zero-copy send for large buffers. The gains depend heavily on the workload and the machine, so measure with your own traffic.

### Should You adopt it in your project

UringNet was designed for IoT gateways: many connections, small messages, request/response traffic. It provides TCP and Unix
stream sockets (no UDP or TLS yet). Consider it if:

- you run Linux 6.0+ (6.16+ for recv bundles) and io_uring is allowed on the host (it is blocked by Docker's and Kubernetes'
  default seccomp profiles), and
- latency or system-call overhead matters to you, and you can spare some CPU for busy polling.

Otherwise, more mature options such as Go [net](https://golang.org/pkg/net/)/[http](https://pkg.go.dev/net/http),
[gnet](https://github.com/panjf2000/gnet), [netty](https://netty.io/) or [libuv](https://github.com/libuv/libuv) are a safer
choice. They run on more platforms and kernels, and have been in production for years.

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
| `URingNet.SpinTime` | busy-poll before blocking (e.g. `50*time.Microsecond`): lower latency at the cost of CPU. Adaptive by default: it stops when the loop is busy |
| `URingNet.SpinBusyBatch` / `SpinIdleBatch` | adaptive polling thresholds: stop polling at this average batch size per loop iteration, resume below the other (defaults 128 / 64) |
| `URingNet.SpinFixed` | always poll, even under full load |
| `URingNet.ZeroCopyThreshold` | enable zero-copy send (`IORING_OP_SEND_ZC`) for writes of at least this many bytes; 0 disables |
| `URingNet.RecvBundle` | recv bundles: one completion can carry several receive buffers. `BundleAuto` (default) enables them on Linux 6.16+ only; see below |
| `URingNet.IncrementalBuffers` | **experimental**: incremental buffer ring (Linux 6.12+), best with `BufferSize` 64KB and 32+ buffers; see below |
| `SetLoops(rings, bufferCount)` | create the event loops; `bufferCount` (receive buffers per ring) is rounded up to a power of 2 (max 32768). See *Sizing receive buffers* below |
| `UserData.WriteNoCopy(b, release)` | queue `b` without copying; `release` is called once the kernel no longer uses it |
| `Ringloop.RunMany()` / `Wait()` / `Shutdown()` | start accepting / block until shut down / close all connections and loops |
| `URingNet.Post(fn)` | run `fn` on the ring's event-loop thread (from any goroutine) |

Callback semantics:

- All callbacks except `OnTick` run on the ring's event-loop thread and must not block.
- `UserData` is per connection and lives for the whole connection; use `UserData.Context` for per-connection state.
- Actions: `Echo`/`Write` send `WriteBuf`; `EchoAndClose` sends then closes; `Close` closes; `Shutdown` shuts down the engine; `None`/`Read` do nothing (receiving is always active).
- Writes on one connection are ordered; short writes are handled. Everything queued during one callback is sent together
  after it returns, and separate segments (for example `WriteNoCopy` data plus a `WriteBuf` header) go out in one `sendmsg`.
- `OnClose` is called exactly once per connection, including peer close, errors and `Shutdown`.

### Sizing receive buffers

Each ring receives into a pool of `bufferCount` buffers of `BufferSize` bytes (a provided buffer ring).

- **Not fewer than the connections that are active at the same time on one ring.** When the pool runs dry, the kernel ends
  the connection's multishot recv with `-ENOBUFS` and it has to be re-armed, which costs far more than a copy.
- **Otherwise as few as possible.** The kernel uses the buffers in FIFO order, so the whole pool is the working set.
  A smaller pool keeps the memory the kernel copies into warm in the CPU cache. In the benchmark, 16KB messages with
  8 requests in flight per connection ran about 1.5x faster with 64 buffers than with 1024.
- `256` (the examples' value) is a reasonable default. Use more for many busy connections per ring, fewer for a few
  connections with bulk transfers.

### Recv bundles

With `IORING_RECVSEND_BUNDLE` one multishot recv completion can fill several consecutive receive buffers, so data that
piled up in the socket costs one completion instead of one per buffer. Each buffer is still passed to `OnTraffic`
separately. In the benchmark on Linux 6.17, bundles improved pipelined 16KB echo by 6–13%.

Kernels 6.10–6.15 have bugs that can leave **gaps in the received data** when bundles are used (fixed upstream by
`3a08988123c8` in 6.15 and `26ec15e4b0c1`/`178b8ff66ff8` in 6.16, all tagged for stable). We reproduced the corruption
on 6.10 and verified correct data on 6.17. Therefore `BundleAuto` only enables bundles on 6.16+. `BundleOn` forces them
(only if you know your kernel has the fixes), and `BundleOff` disables them.

### Incremental buffers (experimental)

`IncrementalBuffers = true` registers the receive ring with `IOU_PBUF_RING_INC` (Linux 6.12+). Instead of using one whole buffer
per receive, the kernel appends each receive after the previous data in the same buffer, so a few large buffers are packed
with data from many connections. Each in-place send references a slice of a buffer; a buffer goes back to the kernel once
the kernel has finished filling it and no slice is referenced. Because a referenced buffer can still receive data, one
connection can send several received slices in place, which avoids most user-space copies.

```go
for _, rn := range ringNets {
	rn.IncrementalBuffers = true
	rn.BufferSize = 64 << 10
}
loop, err := uringnet.SetLoops(ringNets, 32) // 32 x 64KB per ring
```

- In the Linux 6.17 benchmark this was the fastest uringnet configuration: ahead of gnet with 512 connections (small messages
  by ~15%), and within 5–10% of gnet for bulk pipelined transfers (see [Benchmark](#benchmark)).
- Use at least 32 buffers: with 16 buffers, 512 connections sending 16KB each ran out of buffers and slowed down.
- Recv bundles are not used in this mode. On kernels without support it falls back to the normal buffer ring.
- It is new code and off by default; validate it with your own traffic before enabling it in production.

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

### Testing on macOS

```shell
docker run --rm --security-opt seccomp=unconfined -v "$PWD":/src -w /src golang:1.22 go test -race .
```

`seccomp=unconfined` is required because Docker's default seccomp profile blocks io_uring.

## Benchmark

Echo benchmark from [`bench/`](bench/README.md): one client keeps `c` connections open and on each one sends `p` requests of
`s` bytes, then reads all responses back. Servers compared:

- **epoll**: hand-written epoll event loop (one locked thread per core, `SO_REUSEPORT`, non-blocking read/write)
- **gnet**: [gnet](https://github.com/panjf2000/gnet) v2, an epoll framework
- **uringnet**: this project with default settings (256 receive buffers of 16KB; recv bundles on, since the kernel is 6.16+)
- **uringnet + SpinTime**: the same with `SpinTime = 50µs`

> **Environment:** Lima VM (Apple Virtualization.framework) on an Apple Silicon Mac, Ubuntu 25.10, **Linux 6.17**, 4 vCPUs.
> The server is pinned to **1 core** (CPU 0, `GOMAXPROCS=1`, one event loop) and the client to the other 3 cores, over loopback.
> Each number is the median of 3 runs of 3 seconds. Numbers in parentheses are relative to epoll.

### Request/response (`p = 1`)

| Connections, size | epoll | gnet | uringnet | uringnet + SpinTime |
|---|---|---|---|---|
| 1, 128B | 8k | 1.05 | 1.11 | **1.52** |
| 1, 16KB | 7k | **1.21** | 1.06 | 1.13 |
| 16, 128B | 106k | 1.11 | 0.87 | **1.37** |
| 16, 16KB | 77k | 1.03 | 0.81 | **1.27** |
| 64, 128B | 200k | 1.17 | 1.00 | **1.21** |
| 64, 16KB | 145k | **1.10** | 0.97 | 1.06 |
| 512, 128B | 361k | 0.97 | 1.01 | **1.06** |
| 512, 16KB | 187k | 1.00 | 1.09 | **1.11** |

Median latency (p50) and server CPU:

| Connections, size | epoll | gnet | uringnet | uringnet + SpinTime |
|---|---|---|---|---|
| 1, 128B | 79µs / 25% | 75µs / 22% | 80µs / 27% | **34µs** / 61% |
| 16, 128B | 108µs / 63% | 87µs / 62% | 120µs / 55% | **56µs** / 76% |
| 16, 16KB | 155µs / 69% | 135µs / 65% | 204µs / 66% | **96µs** / 83% |
| 64, 128B | 213µs / 82% | 191µs / 86% | 225µs / 78% | **169µs** / 86% |
| 512, 128B | 1229µs / 97% | 1263µs / 98% | 1152µs / 94% | **1087µs** / 94% |

### Pipelined (`p = 8` requests in flight per connection)

| Connections, size | epoll | gnet | uringnet, no bundles | uringnet (bundles) |
|---|---|---|---|---|
| 64, 128B | 1.68M | **1.11** | 0.98 | 1.01 |
| 64, 16KB | 196k | **1.70** | 1.41 | 1.50 |
| 512, 128B | 2.39M | 0.99 | **1.07** | 1.04 |
| 512, 16KB | 154k | **1.82** | 1.43 | 1.61 |

### Incremental buffers (experimental)

Same setup; uringnet with `IncrementalBuffers`, 32 x 64KB buffers per ring:

| Connections, `p`, size | epoll | gnet | uringnet (bundles, 64 x 16KB) | uringnet, incremental |
|---|---|---|---|---|
| 64, 1, 128B | 197k | 1.16 | 1.10 | **1.19** |
| 64, 1, 16KB | 152k | **1.04** | 0.93 | 0.92 |
| 64, 8, 128B | 1.69M | **1.14** | 1.01 | 1.09 |
| 64, 8, 16KB | 205k | **1.68** | 1.51 | 1.60 |
| 512, 1, 128B | 340k | 0.99 | 0.97 | **1.16** |
| 512, 1, 16KB | 215k | 0.99 | 0.96 | **1.05** |
| 512, 8, 128B | 2.66M | 1.00 | 0.96 | **1.14** |
| 512, 8, 16KB | 172k | **1.75** | 1.52 | 1.57 |

Median latency at 512 connections, `p = 8`, 16KB: epoll 23.3ms, gnet 12.3ms, uringnet with bundles 22.4ms, incremental 10.9ms.

### Adaptive busy polling

`SpinTime` polling is adaptive: every 10ms the event loop computes how many completions it handles per iteration on average.
It stops polling when that reaches `SpinBusyBatch` (default 128) and resumes below `SpinIdleBatch` (default 64). At low load
an iteration handles a few completions, so polling saves the sleep/wake-up. Under full load hundreds pile up per iteration;
blocking lets data accumulate into larger batches, while polling splits them up. (The *Request/response* table above was
measured with always-on polling, before polling became adaptive.)

Same setup, incremental buffers (32 x 64KB), `SpinTime = 50µs`:

| Connections, `p`, size | epoll | gnet | no polling | always polling | adaptive polling |
|---|---|---|---|---|---|
| 1, 1, 128B | 10k | 1.05 | 0.98 | **1.67** | 1.60 |
| 16, 1, 128B | 103k | 1.09 | 0.90 | 1.45 | **1.52** |
| 16, 1, 16KB | 81k | 0.94 | 0.72 | **1.12** | 1.06 |
| 16, 8, 16KB | 144k | **1.59** | 1.22 | 1.41 | **1.58** |
| 64, 1, 128B | 201k | 1.24 | 1.00 | **1.26** | 1.21 |
| 64, 1, 16KB | 152k | **1.06** | 0.69 | 0.90 | 0.90 |
| 64, 8, 16KB | 190k | **1.77** | 1.61 | 1.61 | 1.58 |
| 512, 1, 128B | 319k | 0.89 | 1.13 | 1.13 | **1.19** |
| 512, 1, 16KB | 199k | 0.97 | **1.00** | 0.81 | 0.93 |
| 512, 8, 16KB | 136k | **2.13** | 1.84 | 1.87 | 1.78 |

Adaptive polling keeps the low-load gains (median latency 78µs → 30µs at one connection) and recovers most of what always-on
polling loses under full load (512 connections, 16KB: 0.81x → 0.93x of epoll; 1.00x without polling).

### Reading the results

- **When the server is busy** (512 connections, server CPU ≥ 94%), uringnet matches or slightly beats epoll and gnet.
- **At low and medium load**, request/response throughput is bounded by latency, not CPU. Without busy polling, the event loop
  sleeps and wakes up more often than epoll per request, so it trails epoll by up to ~20%. With `SpinTime = 50µs` it leads both
  epoll and gnet and halves the median latency. The cost is CPU: about twice the server CPU at the lowest load, and almost nothing
  extra once the server is busy. A 10µs spin was mostly too short to make a difference in this environment.
- **Incremental buffers** close most of the remaining gap: with them uringnet leads at 512 connections and is within 5–10%
  of gnet for bulk pipelined transfers.
- **Bulk pipelined transfers** are where gnet is strongest. gnet reads up to 64KB per connection into one buffer that stays hot
  in the CPU cache, and only when the event loop gets to that connection. io_uring provided buffers are filled by the kernel as
  soon as data arrives, so every connection with pending data holds a buffer. Recv bundles narrow the gap (1.43x → 1.61x of epoll at
  512 connections). gnet's throughput here comes partly at the expense of fairness: in earlier runs its p99 latency was 450ms+,
  versus 40–70ms for uringnet.
- **Zero-copy send** is not shown: traffic to a local socket is always copied by the kernel, so it has to be measured across
  machines on a real NIC.

### Caveats

These are loopback numbers in a VM. Waking an idle vCPU is expensive in a VM, which favors busy polling more than bare metal
would. With only 3 client cores, differences at high concurrency may be compressed. Results vary by 5–15% between runs, so treat
differences under 10% as a tie.

The same benchmark in Docker Desktop (Linux 6.10, recv bundles disabled, 1-core server with an 8-core client, without
`SpinTime`) was less favorable to uringnet: 0.67–0.78x of epoll at 64 connections, 0.78–0.91x at 512 connections, and
1.29–1.54x for pipelined 16KB (gnet: 1.56–1.69x).

To reproduce, or better, to measure on your own hardware:

```shell
bench/docker.sh                               # in Docker, server 4 cores / client 4 cores
MODE=onecore DEPTHS="1 8" bench/docker.sh     # server 1 core / client 8 cores
sh bench/scripts/run.sh                       # directly on a Linux machine (needs taskset and 12 CPUs)
```

## Known Issues

> **Warning:** The project is still in development. If you find any bugs or have any suggestions, please feel free to open an issue.

- Requires Linux 6.0+ with io_uring enabled. Docker's default seccomp profile and Kubernetes `RuntimeDefault` block io_uring,
  and Linux 6.6+ can disable it with `kernel.io_uring_disabled`. There is no epoll fallback yet.
- Only stream sockets (TCP, Unix) are supported; UDP and TLS are not.
- Recv bundles need Linux 6.16+ for correct data (see *Recv bundles*).
- The send queue of a connection is unbounded: a peer that never reads lets the server buffer data without limit.
- If accept keeps failing (for example `EMFILE`), the multishot accept is re-armed immediately and can spin.
