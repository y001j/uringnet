# Architecture

This document describes how uringnet is built: the layers, the threading model, how data moves through a
connection, and why some things are done the way they are. It is meant for contributors and for users who
want to understand performance or failure behaviour. The [README](../README.md) covers usage.

## Layers

```
  Listen / NewClient / ListenPacket        Server, Client, PacketServer: options, Serve, Shutdown, Stats, Dial
            │
  Handler + *Conn   Framed(MessageHandler)   PacketHandler      public callbacks
  EventHandler + UserData (deprecated)
            │
  connHandler                               internal: apiHandler / legacyHandler adapt the two callback APIs
            │
  conn (connection.go)                      per-connection state machine: receive, send queue, close
            │
  URingNet (uringnet.go)                    one "ring": an event loop, its buffers, its connections, timers
            │
  eventLoop + bufRing (backend.go)          backend interface
     ├─ uringLoop → iouring-go Loop         io_uring: multishot accept/recv/recvmsg, provided buffer ring, SEND_ZC
     └─ epollLoop (epoll.go)                fallback: the same operations emulated with non-blocking sockets
```

Both callback APIs drive the same `conn` state machine, and the state machine only talks to the `eventLoop`
interface, so features (backpressure, timeouts, dialing, UDP) work identically on both backends.

## Threading model

- A server has N rings (`WithRings`, default one per CPU). Each ring owns one event loop running on one
  locked OS thread. With `SO_REUSEPORT` (the default) each ring has its own listening socket and the kernel
  spreads new connections across them; a connection stays on its ring for its whole life.
- Everything about a connection happens on its ring's thread: callbacks, the send queue, buffer accounting.
  None of this state is locked. Callbacks must therefore not block.
- Other goroutines reach a ring with `Post(fn)`, which queues `fn` and wakes the loop (eventfd).
  `Conn.AsyncWrite`, `AsyncClose`, `Dial`, `Server.Stats` and `Shutdown` are built on it.
- `Conn.DialAsync` dials on the caller's ring, so two connections that talk to each other (a proxy) can call
  each other's `Write` directly from their callbacks.

## The io_uring event loop

The `Loop` of [iouring-go](https://github.com/y001j/iouring-go) is a completion-callback loop: each submitted
request carries a handler, and each completion (CQE) calls it with the result and flags. One iteration posts
queued `Post` functions, submits everything prepared and waits for completions with a single `io_uring_enter`,
then dispatches the completions. With Linux 6.1+ the ring uses `SINGLE_ISSUER | DEFER_TASKRUN`, so the kernel
does completion work only when the loop enters.

Optional busy polling (`WithBusyPoll`) spins on the completion queue before blocking. Waking a sleeping thread
dominates latency at low load (especially in VMs); at full load the loop is never idle and polling only splits
batches, so it turns itself off when the average batch per iteration is high (adaptive thresholds).

## Receive path

```
  socket ──multishot recv──► provided buffer ring (N buffers of BufferSize)
                                   │ CQE: bid, length, F_MORE
                                   ▼
                     conn.onRecv → deliver → OnTraffic(c, data)   data = buffer[:n], valid during the call
                                   │
                    held by an in-place send? ──no──► Recycle(bid): back to the kernel right away
                                   │yes
                                   ▼
                     returned when the send completes (or the zero-copy notification arrives)
```

- One multishot recv per connection keeps receiving until it ends (EOF, error, or buffers ran out); the kernel
  picks a buffer from the shared ring for each receive. There are no per-connection read buffers.
- **Running out of buffers** (`-ENOBUFS`) ends the multishot recv. If buffers are held by sends in flight, the
  connection waits in `starved` and is re-armed as buffers come back (one connection per returned buffer, to
  avoid a thundering herd; all of them once nothing is held). Otherwise it is re-armed at once.
- **Buffer count**: the ring is FIFO, so all buffers are the working set; fewer buffers keep the memory the
  kernel copies into warm in the CPU cache. Use at least as many as connections active at once on a ring.
- **Recv bundles** (Linux 6.16+, `BundleAuto`): one completion covers several consecutive buffers. Disabled on
  6.10–6.15, where bundles can leave gaps in the data (reproduced; fixed upstream).
- **Incremental buffers** (6.12+, experimental): a few large buffers are filled by consecutive receives from
  many connections. Each in-place send takes a reference on its buffer; a buffer goes back once the kernel has
  finished filling it (`more == false`) and no reference remains (`incRefs`, `incDone`, `incPinned`).

## Send path

```
  Conn.Write(b) ──► send queue of segments ──► one send / sendmsg in flight ──► completion: drop sent bytes,
   (in a callback:                               (up to 16 segments gathered)     short write: resend the rest
    sent after it returns)
```

- A connection has at most one send in flight, which keeps the byte stream ordered and makes short writes
  simple (resend from the offset). Everything written during one callback is sent together after it returns;
  consecutive segments go out in one `sendmsg`.
- **Copies**: `Write` copies into a reusable buffer and appends small writes to the last copied segment.
- **In-place**: if `b` lies inside the buffer of the current `OnTraffic` call (echo, forwarding) and the queue is
  empty, the receive buffer itself is queued and returned after the send. At most half of the buffers may be
  held this way (`maxHeld`), because running out of buffers costs far more than a copy.
- **Zero copy** (`WithZeroCopy`): segments above the threshold use `SEND_ZC`. The kernel sends from user memory
  and reports with a second, notification CQE when it no longer needs it; only then is the buffer returned or
  `release` called (`zcRef`). The receive ring is registered as a fixed buffer so in-place zero-copy sends avoid
  per-send page pinning. On `EOPNOTSUPP`/`EINVAL`/`ENOMEM` the ring turns zero copy off and resends normally.
  Loopback traffic is always copied by the kernel, so zero copy only pays off across a real NIC.

## Closing

```
  close()  ── shutdown(fd, SHUT_RDWR) ──► pending recv ends (EOF), pending send fails
                                       ──► both ended? ──► close(fd), OnClose(err)  (free)
```

The fd is closed only after every request on it has completed. Closing earlier would let the kernel reuse the
fd number for a new connection while a completion for the old one is still on its way. `Conn.Close` and the
`Close` action first wait for the queued data to be sent (`closeAfter`). When the peer half-closes (EOF), the
data already written is still sent before closing. `OnClose` receives the first recorded reason (`c.err`).

## Flow control and timeouts

- **Backpressure** (`WithMaxWriteBuffer`, default 4MB): after a callback, if the bytes queued on a connection
  exceed the limit, its multishot recv is cancelled, so `OnTraffic` stops and TCP flow control stops the peer.
  Receiving resumes when the queue drains below half.
- **Timeouts** (`WithIdleTimeout`, `WithWriteTimeout`): one timer per ring ticks every quarter of the shortest
  timeout. Connections record only the tick number of their last activity (a plain integer, no clock read on
  the data path); each tick closes connections whose last activity or send progress is too old. They close
  between 1x and 1.25x the timeout.
- **Accept backoff**: a failed multishot accept (for example `EMFILE`) ends; re-arming immediately would fail
  again at once and spin, so it is re-armed after 5ms, doubling up to 1s, and reset by a successful accept.
  Note that the kernel records `RLIMIT_NOFILE` when an accept is submitted.
- **Graceful shutdown**: `Shutdown(ctx)` stops accepting (`shutdown` on the listening sockets), marks every
  connection to close after flushing, waits until all are gone or `ctx` ends, then closes the rest.

## Outgoing connections, UDP and framing

- **Dial**: a socket is created on the ring and connected with `IORING_OP_CONNECT`; name resolution runs on a
  separate goroutine. On success the fd becomes a normal connection (`openConn`), with the caller's context set
  before `OnOpen`. A dial abandoned by its context is cancelled, and a late result is closed without `OnOpen`.
- **UDP**: one socket per ring (`SO_REUSEPORT`), multishot `recvmsg` into the provided buffer ring. Each buffer
  holds `io_uring_recvmsg_out` (16 bytes), the source address (room for a `sockaddr_in6`) and the payload,
  parsed in place into a `netip.AddrPort`, so receiving allocates nothing. Sends use `sendmsg` with the header,
  iovec, address and payload in one allocation kept alive until the completion.
- **Framing**: `Framed` sits between the connection and the user's `MessageHandler`. Frames that arrive whole
  are passed straight from the receive buffer; a partial frame is copied into a per-connection buffer
  (`Conn.pending`) and completed by later data.

## TLS

A TLS connection (`WithTLS`, `WithDialTLS`) is handled in `tls.go` in two phases.

1. **Handshake** with `crypto/tls` on a goroutine of its own, over a transport that reads and writes the socket
   directly (through a `dup` of the fd, so the loop can close the original at any time). Reads never go past the
   end of the current TLS record: everything the peer sends after the handshake stays in the socket. The event
   loop does not touch the socket meanwhile. `OnOpen` runs only after the handshake succeeded; a `Dial` returns
   only then.
2. **Records**, after the handshake:
   - **Kernel TLS** (TLS 1.3 with AES-GCM or ChaCha20-Poly1305, `tls` module loaded): the connection's
     `KeyLogWriter` captured the traffic secrets; HKDF-Expand-Label derives key and IV, and `setsockopt(TCP_ULP,
     "tls")` / `SOL_TLS TLS_RX, TLS_TX` hand them to the kernel with record sequence number 0. That number is
     known only because servers send no session tickets under kTLS (Go sends them with the application keys in
     its first flight) and because the handshake reads stopped at the record boundary. `TLS_RX_EXPECT_NO_PAD`
     lets the kernel decrypt straight into the receive buffer. From then on the connection's data path is that of
     plain TCP. A non-data record (an alert, a session ticket from a server) makes a receive fail with `EIO`; it
     is read with `recvmsg` and its `TLS_GET_RECORD_TYPE` control message: `close_notify` ends the connection like
     EOF, tickets are ignored, `KeyUpdate` (unsupported by the kernel) closes it. `close_notify` is sent with a
     `TLS_SET_RECORD_TYPE` control message before the socket is shut down.
   - **User space** otherwise: `crypto/tls` runs on the loop thread. The transport turns non-blocking and returns
     a temporary `net.Error` when a record is incomplete, which `crypto/tls` does not treat as fatal after the
     handshake (it keeps the partial record). Received ciphertext is only lent to `crypto/tls`, and `Write`
     encrypts straight into the send queue, reusing send buffers.

Backpressure and timeouts work as for plain connections.

## The epoll backend

When io_uring is unavailable (kernel before 6.0, blocked by seccomp, `kernel.io_uring_disabled`), `BackendAuto`
uses `epollLoop`, which implements the same `eventLoop` operations with non-blocking sockets and
level-triggered epoll:

- multishot accept, recv and recvmsg keep reading while the fd is readable (recv stops after a short read,
  since the socket is then most likely drained);
- receives take buffers from an emulated pool (a LIFO free list, so recently returned buffers are reused while
  cache-hot) and report `-ENOBUFS` when it is empty, so the starvation logic above applies unchanged;
- sends are tried immediately and retried on `EPOLLOUT` after `EAGAIN`; connect completes on `EPOLLOUT`;
- timers live in a heap that bounds `epoll_wait`;
- every completion is queued and delivered after the submitting call returns, exactly as with io_uring, so the
  state machine is never re-entered from inside a submission.

Zero-copy send, recv bundles and incremental buffers are io_uring features and are disabled on epoll. In the
benchmark the epoll backend reaches 0.77–1.18x of a hand-written epoll loop.

## Memory

- Receive buffers: `count × size` per ring (default 256 × 16KB = 4MB), allocated once.
- io_uring rings count against `RLIMIT_MEMLOCK` per user for processes without `CAP_IPC_LOCK`: about 70KB per
  ring with the default SQ size of 512 (550KB at 4096). Registered fixed buffers (zero copy) count too.
- Per connection: the `conn` struct, a send queue that is reused once empty, and up to two reusable copy
  buffers. Connections hold no receive memory of their own.

## Testing

- Unit and integration tests run real servers over loopback, under the race detector, on both backends
  (`URINGNET_BACKEND=epoll` runs the whole suite on epoll; CI does both, on Go 1.21 and the latest release).
- Each hardening feature has a test that fails when the feature is disabled (accept backoff, backpressure,
  timeouts, half-close).
- `FuzzEcho` sends fuzzed data in fuzzed chunks to three server configurations (scarce buffers, incremental
  buffers, zero copy) and requires the echo to be identical.
- `TestStressChurn` runs thousands of connections with echo, RST, half-close and write-only sessions and then
  checks for leaked connections, buffers and fds.
- Benchmarks live in `bench/` (separate module); see `bench/README.md`.
