# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before v1.0.0, minor versions may change the API.

## [v0.5.0] - 2026-11-24

IoT protocols on top of the core: an MQTT broker (3.1.1 and 5), Modbus TCP / RTU over TCP, and a
Modbus-to-MQTT gateway that runs in one process. No breaking changes.

### Added
- `mqtt` package: an MQTT 3.1.1 broker (QoS 0–2 from publishers, wildcards, retained messages, wills, persistent
  sessions, takeover, keepalive, authentication and authorization hooks, `OnPublish`, `Publish`, `Stats`), and
  `example/mqttbroker`. 1.75–2.67x the throughput of Mosquitto 2.0.22 on one core in three scenarios.
- `mqtt`: MQTT 5 alongside 3.1.1, with clients of both versions talking to each other: properties, reason codes
  on acknowledgements and server-initiated DISCONNECTs, session and message expiry, user and other message
  properties forwarded to MQTT 5 subscribers, topic aliases, receive maximum, maximum packet size, assigned client
  IDs, subscription options (No Local, Retain As Published, Retain Handling), will delay. Shared subscriptions,
  subscription identifiers and enhanced authentication are refused with their reason codes. New
  `Broker.PublishMessage`, `Message.Expiry`, `Message.Properties`, `Stats.MessagesExpired`, `Stats.SessionsExpired`.
- `mqtt`: `Config.SessionExpiry` removes sessions offline for longer (MQTT 3.1.1 persistent sessions never expired
  before) and caps the expiry MQTT 5 clients ask for; `Config.TopicAliasMaximum`.
- `Conn.SameLoop` and `Conn.Post`.
- `modbus` package: Modbus TCP and RTU over TCP (functions 0x01–0x06, 0x0F, 0x10, 0x17, exceptions). `Client`, a
  master with context-aware requests, pipelining, timeouts, retries and reconnection; `DTUServer`, a master for
  DTUs and serial device servers that connect in, with registration and heartbeat packets; `Server`, a slave with
  the `Memory` data model and `Units` routing; `example/modbusdtu`.
- `modbus`: point tables (`Point`: table, address, data type, byte order, bit, string length, scale, offset,
  interval; JSON) with `DecodeRegisters`/`EncodeRegisters`; `Poller`, which merges adjacent points into requests,
  schedules all devices from one goroutine with asynchronous requests, skips overrunning polls, reports
  `ErrOffline` and writes points.
- `gateway` package: publishes polled values to an `mqtt.Broker` (per point or per device, retained, only on
  change), device status, and opt-in write commands answered on a result topic or the MQTT 5 response topic;
  `example/gateway`, configured by JSON with point-table templates and automatic DTU polling.
- `mqtt.Broker.Subscribe`: in-process subscriptions.

## [v0.4.0] - 2026-11-23

Closes gaps with gnet and the standard library found in a comparison. No breaking changes.

### Added
- `WithLoadBalancing`: `BalanceKernel` (default), `BalanceRoundRobin`, `BalanceLeastConnections`,
  `BalanceSourceAddrHash`.
- Incremental reading: `Conn.Peek`, `Conn.Next`, `Conn.Discard`, `Conn.InboundBuffered`.
- `WithTCPKeepAlive`, `WithSocketBuffers`.
- `WithSlowCallbackWarning` with a watchdog for callbacks that do not return; `Stats.SlowCallbacks`.
- UDP multicast: `ListenPacket` joins a multicast group address; `WithMulticastInterface`.

### Changed
- epoll backend: completions run after each fd's events instead of after the whole `epoll_wait` batch, recv
  uses `readv` into two buffers to detect a drained socket without an extra read, and in-place sends may hold
  every buffer. Median throughput against a hand-written epoll loop rose from 0.83x to 0.94x (small messages at
  16–64 connections from 0.71–0.81x to 0.92–0.99x).

## [v0.3.0] - 2026-11-22

Adds an epoll fallback and TLS with kernel TLS offload. No breaking changes.

### Added
- epoll backend: `WithBackend`, `Backend` (`BackendAuto` falls back to epoll when io_uring is unavailable, with a
  warning; `BackendIOUring`, `BackendEpoll`), `Server.Backend`, `PacketServer.Backend`.
- TLS: `WithTLS`, `WithDialTLS`, `Conn.TLSConnectionState`, `Stats.TLSHandshakeErrors`.
- Kernel TLS (kTLS) after TLS 1.3 handshakes: `WithKTLS` (default on), `Conn.KernelTLS`,
  `Stats.KernelTLSConnections`. Needs the `tls` kernel module; falls back to `crypto/tls` in user space.
  Servers using kTLS send no session tickets.
- `docs/ARCHITECTURE.md`; all code comments are in English.

## [v0.2.0] - 2026-11-22

Adds outgoing connections, UDP and message framing. No breaking changes: code written for v0.1.0
builds unchanged.

### Added
- Outgoing connections: `Server.Dial`, `Server.DialAsync`, `Conn.DialAsync` (same event loop, for proxies) and
  `NewClient` for programs that only dial. `Stats.Dialed`.
- UDP: `ListenPacket`, `PacketServer`, `PacketConn` with multishot `recvmsg`; source addresses as `netip.AddrPort`.
  UDP counters in `Stats`.
- Framing: `Framer`, `Framed`, `MessageHandler`, and the `NewLengthField`, `NewDelimiter` and `FixedLength` framers.
  `ErrFrameTooLarge`.
- `Server.Stats` and pkg.go.dev examples.

### Fixed
- `Listen("unix", ...)` failed with "setsockopt: operation not supported" because `SO_REUSEPORT` was set on the unix socket.

## [v0.1.0] - 2026-11-22

The core was rebuilt on the `Loop` API of [iouring-go](https://github.com/y001j/iouring-go)
and gained a new public API. **Requires Linux 6.0 or later.**

### Added
- `Listen` / `Server` with functional options (`WithRings`, `WithBuffers`, `WithBusyPoll`,
  `WithZeroCopy`, `WithIdleTimeout`, `WithWriteTimeout`, `WithMaxWriteBuffer`, `WithLogger`, ...),
  `Serve`, graceful `Shutdown(ctx)` and `Close`.
- `Handler` with a `*Conn` per connection: `Write` (in place when the bytes lie in the received
  buffer), `WriteNoCopy`, `Close` (after queued data is sent), `AsyncWrite` / `AsyncClose` from any
  goroutine, `Context`, addresses. `OnClose` receives the reason (`nil`, `io.EOF`, `ErrIdleTimeout`,
  `ErrWriteTimeout`, `ErrServerClosed` or the socket error).
- Event loop on iouring-go `Loop`: one io_uring and one locked OS thread per ring, multishot
  accept/recv, provided buffer ring, one `io_uring_enter` per iteration,
  `SINGLE_ISSUER | DEFER_TASKRUN` on 6.1+.
- Adaptive busy polling, recv bundles (enabled on 6.16+ only, see README), incremental buffer rings
  (6.12+, experimental), zero-copy send (`SEND_ZC`, fixed buffers), batched `sendmsg` sends.
- Per-connection send queue limit with backpressure, idle and write timeouts, exponential backoff
  when accept fails, `log/slog` logging, startup checks with `ErrKernelTooOld` and
  `ErrIOUringUnavailable`.
- `FuzzEcho` fuzz test and `TestStressChurn` leak/stress test.
- Benchmark suite in `bench/` (single machine and cross-machine), results and charts in the README,
  Chinese README, roadmap (`docs/ROADMAP.md`).
- CI on Go 1.21 and stable with the race detector, staticcheck and coverage.

### Changed
- Minimum Go version is 1.21.
- Depends on `github.com/y001j/iouring-go` v0.1.0 and `golang.org/x/sys` v0.30.0.
- When the peer half-closes a connection, data already written is sent before closing.
- With port 0, `SO_REUSEPORT` and several rings, all rings bind the same port.

### Deprecated
- `New`, `NewMany`, `SetLoops`, `Ringloop`, `EventHandler`, `BuiltinEventEngine`, `UserData`,
  `NetAddress` and the `Echo`, `Read`, `Write` and `EchoAndClose` actions. They still work and will
  be removed in a later release; use `Listen`, `Handler` and `Conn`.
- The `sqpoll` argument of `New`/`NewMany` is ignored.

### Removed
- The legacy `uring/` package.
- The unimplemented `Conn`, `Reader`, `Writer`, `Socket` and `AsyncCallback` interfaces.

### Fixed
- `Listen` uses a submission queue of 512 entries by default, about 70KB of locked memory per ring.
  A queue of 4096 locked about 550KB per ring, so with the common 8MB `RLIMIT_MEMLOCK` only 15 rings
  could be created and the default (one ring per CPU) failed on machines with 16 or more cores.
- Accept no longer spins at full CPU when it fails (for example `EMFILE`).
- A peer that never reads can no longer make the server buffer data without limit.
- `example/httpserver` honours HTTP/1.0 and `Connection: close` (#14).

## [v0.1.0-alpha] - 2026-11-22

Pre-release of the rebuilt core with the hardening above, before the API rework.

## [v0.0.3-alpha] - 2023-10-23

Last release of the original implementation.

[v0.5.0]: https://github.com/y001j/uringnet/compare/v0.4.0...v0.5.0
[v0.4.0]: https://github.com/y001j/uringnet/compare/v0.3.0...v0.4.0
[v0.3.0]: https://github.com/y001j/uringnet/compare/v0.2.0...v0.3.0
[v0.2.0]: https://github.com/y001j/uringnet/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/y001j/uringnet/compare/v0.0.3-alpha...v0.1.0
[v0.1.0-alpha]: https://github.com/y001j/uringnet/compare/v0.0.3-alpha...v0.1.0-alpha
[v0.0.3-alpha]: https://github.com/y001j/uringnet/releases/tag/v0.0.3-alpha
