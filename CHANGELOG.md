# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before v1.0.0, minor versions may change the API.

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
- Accept no longer spins at full CPU when it fails (for example `EMFILE`).
- A peer that never reads can no longer make the server buffer data without limit.
- `example/httpserver` honours HTTP/1.0 and `Connection: close` (#14).

## [v0.1.0-alpha] - 2026-11-22

Pre-release of the rebuilt core with the hardening above, before the API rework.

## [v0.0.3-alpha] - 2023-10-23

Last release of the original implementation.

[v0.1.0]: https://github.com/y001j/uringnet/compare/v0.0.3-alpha...v0.1.0
[v0.1.0-alpha]: https://github.com/y001j/uringnet/compare/v0.0.3-alpha...v0.1.0-alpha
[v0.0.3-alpha]: https://github.com/y001j/uringnet/releases/tag/v0.0.3-alpha
