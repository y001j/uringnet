//go:build linux
// +build linux

package uringnet

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	socket "github.com/y001j/uringnet/sockets"
)

// Server is a set of io_uring event loops serving one listening address.
//
//	srv, err := uringnet.Listen("tcp", ":8080", handler, uringnet.WithIdleTimeout(time.Minute))
//	...
//	go func() { <-sig; srv.Shutdown(ctx) }()
//	if err := srv.Serve(); err != uringnet.ErrServerClosed { ... }
type Server struct {
	h     Handler
	log   *slog.Logger
	rings []*URingNet
	loop  *Ringloop
	addr  net.Addr

	served   atomic.Bool
	closed   atomic.Bool
	stopOnce sync.Once
}

// Option configures a Server; see the With* functions.
type Option func(*options)

type options struct {
	rings          int
	sqSize         uint
	bufCount       int
	bufSize        int
	spin           time.Duration
	spinFixed      bool
	spinBusy       int
	spinIdle       int
	zeroCopy       int
	bundle         BundleMode
	incremental    bool
	maxWriteBuffer int
	idleTimeout    time.Duration
	writeTimeout   time.Duration
	reusePort      bool
	noDelay        bool
	logger         *slog.Logger
}

// WithRings sets the number of event loops (default runtime.NumCPU()). Each has its own
// io_uring and locked OS thread.
func WithRings(n int) Option { return func(o *options) { o.rings = n } }

// WithBuffers sets the receive buffers per ring: count (rounded up to a power of two,
// default 256) buffers of size bytes (default 16KB). Use at least as many buffers as
// connections active at once on a ring, otherwise as few as possible: the kernel uses
// them in FIFO order, so fewer buffers stay warm in the CPU cache.
func WithBuffers(count, size int) Option {
	return func(o *options) { o.bufCount, o.bufSize = count, size }
}

// WithSQSize sets the submission queue size per ring (default 4096).
func WithSQSize(n uint) Option { return func(o *options) { o.sqSize = n } }

// WithBusyPoll makes each event loop poll for completions for d before blocking: lower
// latency for more CPU (about 50µs is a good start). Polling stops on its own while the
// loop is fully loaded; see URingNet.SpinBusyBatch for the thresholds.
func WithBusyPoll(d time.Duration) Option { return func(o *options) { o.spin = d } }

// WithBusyPollFixed polls for d before blocking regardless of load.
func WithBusyPollFixed(d time.Duration) Option {
	return func(o *options) { o.spin, o.spinFixed = d, true }
}

// WithBusyPollThresholds tunes adaptive busy polling: polling stops when the event loop
// handles busy completions per iteration on average, and resumes below idle (defaults
// 128 and 64). Higher values keep polling under more load (favouring latency).
func WithBusyPollThresholds(busy, idle int) Option {
	return func(o *options) { o.spinBusy, o.spinIdle = busy, idle }
}

// WithZeroCopy sends writes of at least threshold bytes with SEND_ZC (in-place sends of
// received data and WriteNoCopy). Useful from about 10KB up across a real NIC; never for
// loopback, where the kernel copies anyway.
func WithZeroCopy(threshold int) Option { return func(o *options) { o.zeroCopy = threshold } }

// WithRecvBundle sets whether recv bundles are used (default BundleAuto: Linux 6.16+).
func WithRecvBundle(m BundleMode) Option { return func(o *options) { o.bundle = m } }

// WithIncrementalBuffers enables the incremental buffer ring (Linux 6.12+, experimental);
// combine it with fewer, larger buffers, e.g. WithBuffers(32, 64<<10).
func WithIncrementalBuffers() Option { return func(o *options) { o.incremental = true } }

// WithMaxWriteBuffer limits the bytes queued per connection (default 4MB, negative for no
// limit). Above it the connection stops receiving until the queue drains below half.
func WithMaxWriteBuffer(n int) Option { return func(o *options) { o.maxWriteBuffer = n } }

// WithIdleTimeout closes connections that neither receive nor send data for d.
func WithIdleTimeout(d time.Duration) Option { return func(o *options) { o.idleTimeout = d } }

// WithWriteTimeout closes connections whose queued data makes no progress for d.
func WithWriteTimeout(d time.Duration) Option { return func(o *options) { o.writeTimeout = d } }

// WithReusePort gives each ring its own listening socket with SO_REUSEPORT, so the kernel
// spreads new connections across rings (default true; ignored for unix sockets).
func WithReusePort(on bool) Option { return func(o *options) { o.reusePort = on } }

// WithTCPNoDelay sets TCP_NODELAY on accepted connections (default true).
func WithTCPNoDelay(on bool) Option { return func(o *options) { o.noDelay = on } }

// WithLogger receives warnings about failures the server recovers from (accept errors,
// zero-copy fallback) and debug messages (timeouts). By default nothing is logged.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// Listen creates a Server listening on address. network is "tcp", "tcp4", "tcp6" or
// "unix". The server does not accept connections until Serve is called.
func Listen(network, address string, h Handler, opts ...Option) (*Server, error) {
	if h == nil {
		return nil, fmt.Errorf("uringnet: nil Handler")
	}
	o := options{rings: runtime.NumCPU(), sqSize: 4096, bufCount: 256, bufSize: DefaultBufferSize,
		reusePort: true, noDelay: true}
	for _, opt := range opts {
		opt(&o)
	}
	if err := checkKernel(); err != nil {
		return nil, err
	}
	sockOpts := socket.SocketOptions{ReusePort: o.reusePort}
	if o.noDelay {
		sockOpts.TCPNoDelay = socket.TCPNoDelay
	} else {
		sockOpts.TCPNoDelay = socket.TCPDelay
	}
	rings, err := NewMany(NetAddress{AddrType: socket.NetAddressType(network), Address: address},
		o.sqSize, false, o.rings, sockOpts, nil)
	if err != nil {
		return nil, err
	}
	s := &Server{h: h, log: o.logger, rings: rings}
	if s.log == nil {
		s.log = discardLogger
	}
	for _, rn := range rings {
		rn.h = apiHandler{h: h, s: s}
		rn.BufferSize = o.bufSize
		rn.SpinTime = o.spin
		rn.SpinFixed = o.spinFixed
		rn.SpinBusyBatch = o.spinBusy
		rn.SpinIdleBatch = o.spinIdle
		rn.ZeroCopyThreshold = o.zeroCopy
		rn.RecvBundle = o.bundle
		rn.IncrementalBuffers = o.incremental
		rn.MaxWriteBuffer = o.maxWriteBuffer
		rn.IdleTimeout = o.idleTimeout
		rn.WriteTimeout = o.writeTimeout
		rn.Logger = s.log
	}
	if sa, err := unix.Getsockname(rings[0].SocketFd); err == nil {
		s.addr = sockaddrToNetAddr(sa)
	}
	loop, err := SetLoops(rings, o.bufCount)
	if err != nil {
		closeListeners(rings)
		return nil, err
	}
	s.loop = loop
	return s, nil
}

func closeListeners(rings []*URingNet) {
	closed := map[int]bool{}
	for _, rn := range rings {
		if !closed[rn.SocketFd] {
			closed[rn.SocketFd] = true
			unix.Close(rn.SocketFd)
		}
	}
}

// Addr returns the listening address (with the actual port when listening on port 0).
func (s *Server) Addr() net.Addr { return s.addr }

// Connections returns the number of open connections.
func (s *Server) Connections() int64 {
	var n int64
	for _, rn := range s.rings {
		n += rn.Connections()
	}
	return n
}

// Serve starts accepting connections and blocks until the server is shut down. It always
// returns a non-nil error: ErrServerClosed after Shutdown or Close.
func (s *Server) Serve() error {
	if s.closed.Load() {
		return ErrServerClosed
	}
	if !s.served.CompareAndSwap(false, true) {
		return fmt.Errorf("uringnet: Serve called twice")
	}
	s.h.OnBoot(s)
	s.loop.RunMany()
	s.loop.Wait()
	return ErrServerClosed
}

// Shutdown stops accepting connections, lets every connection send the data already
// written and then closes it, and waits until all connections are closed or ctx is done.
// Connections still open then are closed at once and ctx.Err() is returned. OnClose
// receives ErrServerClosed for connections closed by the shutdown.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.closed.Load() {
		return nil
	}
	if s.served.Load() {
		for _, rn := range s.rings {
			rn.Post(rn.drain)
		}
		t := time.NewTicker(10 * time.Millisecond)
		defer t.Stop()
		for s.Connections() > 0 {
			select {
			case <-ctx.Done():
				s.Close()
				return ctx.Err()
			case <-t.C:
			}
		}
	}
	s.Close()
	return nil
}

// Close closes all connections and event loops at once, without waiting for queued data.
func (s *Server) Close() error {
	s.stopOnce.Do(func() {
		s.closed.Store(true)
		if !s.served.Load() {
			// 没有 Serve 过：Serve 中 Wait 不会被调用，这里直接关闭
			s.served.Store(true)
		}
		s.loop.Shutdown()
		s.h.OnShutdown(s)
	})
	return nil
}
