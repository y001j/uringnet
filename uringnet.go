//go:build linux
// +build linux

package uringnet

import (
	"context"
	"crypto/tls"
	stderrors "errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	iouring "github.com/y001j/iouring-go"
	iouring_syscall "github.com/y001j/iouring-go/syscall"
	"golang.org/x/sys/unix"

	"github.com/y001j/uringnet/errors"
	socket "github.com/y001j/uringnet/sockets"
)

// DefaultBufferSize is the default size of one receive buffer.
const DefaultBufferSize = 16384

// DefaultMaxWriteBuffer is the default per-connection send queue limit; see URingNet.MaxWriteBuffer.
const DefaultMaxWriteBuffer = 4 << 20

var (
	// ErrKernelTooOld is returned when the running kernel is older than Linux 6.0.
	ErrKernelTooOld = stderrors.New("uringnet: Linux 6.0 or later is required")
	// ErrIOUringUnavailable is returned when io_uring cannot be used: it is disabled by a
	// seccomp profile (Docker, Kubernetes RuntimeDefault), by kernel.io_uring_disabled, or
	// not compiled into the kernel.
	ErrIOUringUnavailable = stderrors.New("uringnet: io_uring is not available")
)

// discardLogger is the default logger: it drops everything.
var discardLogger = slog.New(discardHandler{})

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }

// URingNet is one event loop (a "ring") of the deprecated API; Server and Client use it internally.
//
// Each URingNet owns an event loop backend (one io_uring or epoll instance and one locked OS
// thread). All callbacks run on that thread and must not block.
type URingNet struct {
	Addr     string                // listening address
	Type     socket.NetAddressType // network
	SocketFd int                   // listening socket, -1 for a client
	Handler  EventHandler          // callbacks (deprecated API)

	// The fields below must be set before SetLoops.

	// BufferSize is the size of one receive buffer in bytes (default DefaultBufferSize).
	BufferSize int

	// SpinTime is how long the event loop busy-polls for completions before blocking (0: never).
	// It trades CPU for latency and helps most where wake-ups are expensive (VMs) and at low to
	// medium load; 50µs is a good start. Adaptive by default: polling stops while the loop is fully loaded.
	SpinTime time.Duration

	// SpinFixed polls regardless of load (no adaptation).
	SpinFixed bool

	// Adaptive polling thresholds: polling stops when the loop handles SpinBusyBatch completions per
	// iteration on average and resumes below SpinIdleBatch. 0 means the defaults (128 / 64). Higher
	// values keep polling under more load (favouring latency), lower ones stop earlier (throughput, CPU).
	SpinBusyBatch int
	SpinIdleBatch int

	// ZeroCopyThreshold > 0 enables zero-copy send (IORING_OP_SEND_ZC, Linux 6.0+) for in-place sends
	// (WriteBuf pointing into the receive buffer) and WriteNoCopy data of at least this many bytes:
	// the kernel transmits straight from that memory. The receive buffers are also registered as fixed
	// buffers; their pinned pages count against RLIMIT_MEMLOCK, and registration falls back to none,
	// and SEND_ZC to normal sends on ENOMEM. Worth it from about 10KB up; never on loopback, where the
	// kernel copies anyway.
	ZeroCopyThreshold int

	// RecvBundle controls recv bundles (one completion fills several consecutive buffers, which cuts
	// completions and callbacks when data piles up). BundleAuto (default) enables them on Linux 6.16+
	// only: 6.10–6.15 can leave gaps in the received data (fixed upstream by 3a08988123c8, 26ec15e4b0c1,
	// 178b8ff66ff8, possibly backported). BundleOn forces them, for tests or kernels known to be fixed.
	RecvBundle BundleMode

	// IncrementalBuffers (experimental) uses an incrementally consumed buffer ring (Linux 6.12+): a few
	// large buffers are filled by many receives in a row, so a connection can send several received
	// slices in place. Use a large BufferSize (e.g. 64KB) and few buffers. Disables recv bundles; falls back when unsupported.
	IncrementalBuffers bool

	// MaxWriteBuffer limits the bytes queued per connection (backpressure). Above it the connection
	// stops receiving (no OnTraffic) until the queue drains below half, so a peer that does not read
	// cannot make the server buffer without limit: TCP flow control then stops the peer too.
	// 0 means DefaultMaxWriteBuffer, negative means no limit. Data written in one callback is never refused, so the queue may briefly exceed it.
	MaxWriteBuffer int

	// IdleTimeout > 0 closes connections that neither receive nor send data for that long.
	IdleTimeout time.Duration

	// WriteTimeout > 0 closes connections whose queued data makes no progress for that long (a peer
	// that stopped reading). Together with MaxWriteBuffer it clears out stuck or vanished peers.
	WriteTimeout time.Duration

	// Logger receives warnings about recovered failures (accept errors, fallbacks); nil logs nothing.
	Logger *slog.Logger

	// Backend selects the event loop implementation; BackendAuto (default) falls back to epoll when io_uring is unavailable.
	Backend Backend

	index      int
	entries    uint32
	ownsSocket bool // SocketFd belongs to this ring alone (one listening socket per ring with ReusePort)

	h        connHandler // callbacks: the deprecated Handler or a Server's Handler
	log      *slog.Logger
	loop     eventLoop
	br       bufRing
	backend  Backend // backend in use
	ringloop *Ringloop

	// The fields below are only accessed on the loop thread.
	conns       map[int]*conn
	held        int     // receive buffers held by send queues
	maxHeld     int     // maximum buffers held by in-place sends (half of them)
	starved     []*conn // connections whose receiving stopped with -ENOBUFS
	stopping    bool
	draining    bool        // graceful shutdown: no new connections, existing ones close after flushing
	noDelay     bool        // set TCP_NODELAY on dialed connections
	tlsServer   *tls.Config // TLS for accepted connections (WithTLS)
	tlsClient   *tls.Config // TLS for dialed connections (WithDialTLS)
	tlsErrors   uint64      // failed TLS handshakes
	tlsBuf      []byte      // plaintext buffer shared by the ring's TLS connections
	fixedIdx    int         // fixed-buffer index of the receive buffers, -1 if not registered
	zcOff       bool        // zero-copy send turned off (unsupported, or epoll)
	zcSends     int         // SEND_ZC sends (stats, tests)
	bundle      bool        // recv bundles in use
	bundleBufs  []iouring.BundleBuf
	inc         bool    // incremental buffer ring in use
	incRefs     []int32 // incremental: slices of each buffer referenced by in-place sends
	incDone     []bool  // incremental: the kernel has finished filling the buffer
	incPinned   int     // incremental: buffers with at least one referenced slice
	gatherSends int     // sendmsg gather sends (tests)
	maxWrite    int     // effective MaxWriteBuffer, <= 0 for no limit
	pauses      int     // receive pauses caused by backpressure
	nopH        iouring.Handler

	// Timeouts: a tick every tickEvery; connections record the tick of their last activity (a coarse clock, no time reads).
	tick                  uint64
	tickEvery             time.Duration
	idleTicks, writeTicks uint64 // close after this many ticks without activity / send progress; 0 disables

	acceptBackoff time.Duration // wait before re-arming a failed accept, reset after a successful one
	acceptArms    int           // accept submissions (tests)
	timeouts      int           // connections closed by a timeout (tests)

	// Counters for Server.Stats, updated on the loop thread only.
	accepted, acceptErrs    uint64
	bytesIn, bytesOut       uint64
	enobufs                 uint64 // multishot receives ended by -ENOBUFS
	idleCloses, writeCloses uint64
	dialed                  uint64 // outgoing connections established

	// UDP（ListenPacket）
	packet                bool // packet mode: no connections, datagrams via multishot recvmsg
	ph                    PacketHandler
	pc                    PacketConn
	recvMsg               unix.Msghdr // msghdr of the multishot recvmsg (only the name length is used)
	family                int         // address family of the socket (AF_INET / AF_INET6)
	packetsIn, packetsOut uint64
	truncated, sendErrs   uint64

	connCount atomic.Int64

	shutdownOnce sync.Once
}

// UserData is a connection as seen by the deprecated callbacks: one per connection, reused for its lifetime.
//
// Deprecated: use Handler, which receives a *Conn. UserData will be removed in a later release.
type UserData struct {
	Fd int32

	// Buffer holds the data of the current OnTraffic call. It is only valid during the call;
	// its memory goes back to the kernel afterwards, so copy what you need to keep.
	Buffer  []byte
	BufSize int32

	// WriteBuf is data to send to the peer: set it in OnOpen/OnTraffic and return Echo/Write/EchoAndClose.
	// It is copied (or sent in place when it points into Buffer) and reset to length 0 after the callback.
	WriteBuf []byte

	// Context holds user-defined per-connection state.
	Context interface{}

	c *conn
}

// WriteNoCopy queues b without copying it. It must be called from a callback (the event loop thread).
// release (may be nil) is called once the kernel no longer uses b (after the send, or the zero-copy notification); do not modify b before.
// With ZeroCopyThreshold set and b large enough, b is sent with SEND_ZC.
// b is queued at once, so it goes out before WriteBuf of the same callback. On a closed connection release is called at once and an error returned.
func (d *UserData) WriteNoCopy(b []byte, release func()) error {
	return d.c.writeNoCopy(b, release)
}

// RemoteAddr returns the peer address.
func (d *UserData) RemoteAddr() net.Addr {
	sa, err := unix.Getpeername(int(d.Fd))
	if err != nil {
		return nil
	}
	return sockaddrToNetAddr(sa)
}

// LocalAddr returns the local address.
func (d *UserData) LocalAddr() net.Addr {
	sa, err := unix.Getsockname(int(d.Fd))
	if err != nil {
		return nil
	}
	return sockaddrToNetAddr(sa)
}

// BundleMode selects whether recv bundles are used; see URingNet.RecvBundle.
type BundleMode int

const (
	BundleAuto BundleMode = iota // on Linux 6.16+ when the kernel supports them
	BundleOn                     // whenever the kernel supports them (no version check)
	BundleOff                    // never
)

// bundleSafeKernel reports whether the kernel has the recv bundle data-corruption fixes (mainline 6.16).
func bundleSafeKernel() bool {
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return false
	}
	var major, minor int
	fmt.Sscanf(unix.ByteSliceToString(u.Release[:]), "%d.%d", &major, &minor)
	return major > 6 || (major == 6 && minor >= 16)
}

// NetAddress describes a listening address.
//
// Deprecated: Listen takes the network and address directly.
type NetAddress struct {
	AddrType socket.NetAddressType
	Address  string
}

// New creates a single URingNet; set Handler before SetLoops.
//
// size is the SQ size (rounded up to a power of 2 by the kernel). sqpoll is ignored: the loop uses
// SINGLE_ISSUER/DEFER_TASKRUN with one io_uring_enter per iteration and needs no SQPOLL kernel
// thread spinning.
//
// Deprecated: use Listen.
func New(addr NetAddress, size uint, sqpoll bool, options socket.SocketOptions) (*URingNet, error) {
	rings, err := NewMany(addr, size, sqpoll, 1, options, nil)
	if err != nil {
		return nil, err
	}
	return rings[0], nil
}

// NewMany creates num URingNets; start them with SetLoops and Ringloop.RunMany.
//
//	@param size   SQ size, rounded up to a power of 2 by the kernel
//	@param sqpoll ignored, kept for compatibility
//	@param num    number of rings (event loops)
//
// With options.ReusePort each ring gets its own listening socket and the kernel balances new
// connections across them; otherwise all rings share one listening socket.
// Only stream protocols (tcp/tcp4/tcp6/unix) are supported.
//
// Deprecated: use Listen with options (WithRings, WithBuffers, ...).
func NewMany(addr NetAddress, size uint, sqpoll bool, num int, options socket.SocketOptions, handler EventHandler) ([]*URingNet, error) {
	if num <= 0 {
		return nil, fmt.Errorf("uringnet: invalid number of rings %d", num)
	}
	switch addr.AddrType {
	case socket.Tcp, socket.Tcp4, socket.Tcp6, socket.Unix:
	default:
		return nil, errors.ErrUnsupportedProtocol
	}
	if size == 0 {
		size = 4096
	}

	perRing := options.ReusePort && addr.AddrType != socket.Unix
	rings := make([]*URingNet, num)
	var sharedFd = -1
	cleanup := func() {
		for _, r := range rings {
			if r != nil && r.ownsSocket {
				unix.Close(r.SocketFd)
			}
		}
		if sharedFd >= 0 {
			unix.Close(sharedFd)
		}
	}

	for i := 0; i < num; i++ {
		var fd int
		if perRing || i == 0 {
			var err error
			if fd, err = listen(addr, options); err != nil {
				cleanup()
				return nil, err
			}
			if i == 0 && perRing && num > 1 {
				// With port 0 each listening socket would get its own random port: bind the rest to the first one's
				addr.Address = boundAddress(addr.Address, fd)
			}
			if !perRing {
				sharedFd = fd
			}
		} else {
			fd = sharedFd
		}
		rings[i] = &URingNet{
			Addr:       addr.Address,
			Type:       addr.AddrType,
			SocketFd:   fd,
			Handler:    handler,
			index:      i,
			entries:    uint32(size),
			ownsSocket: perRing,
		}
	}
	return rings, nil
}

// boundAddress returns address with fd's actual port if address has port 0, otherwise address unchanged.
func boundAddress(address string, fd int) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "0" {
		return address
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		return address
	}
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		return net.JoinHostPort(host, strconv.Itoa(a.Port))
	case *unix.SockaddrInet6:
		return net.JoinHostPort(host, strconv.Itoa(a.Port))
	}
	return address
}

func listen(addr NetAddress, options socket.SocketOptions) (int, error) {
	ops := socket.SetOptions(string(addr.AddrType), options)
	var (
		fd  int
		err error
	)
	if addr.AddrType == socket.Unix {
		fd, _, err = socket.UnixSocket(string(addr.AddrType), addr.Address, true, ops...)
	} else {
		fd, _, err = socket.TCPSocket(string(addr.AddrType), addr.Address, true, ops...)
	}
	if err != nil {
		return -1, fmt.Errorf("%w: %v", errors.ErrSocketCreation, err)
	}
	if fd < 0 {
		return -1, errors.ErrInvalidSocketFd
	}
	// The fd stays in blocking mode for io_uring, which does the waiting: some kernels return -EAGAIN at once for O_NONBLOCK fds
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("%w: %v", errors.ErrSocketCreation, err)
	}
	return fd, nil
}

// start creates the event loop; called by SetLoops.
func (rn *URingNet) start(bufferCount int) error {
	if rn.h == nil {
		if rn.Handler == nil {
			return fmt.Errorf("uringnet: ring %d has no Handler", rn.index)
		}
		rn.h = legacyHandler{rn.Handler}
	}
	rn.log = rn.Logger
	if rn.log == nil {
		rn.log = discardLogger
	}
	if rn.BufferSize <= 0 {
		rn.BufferSize = DefaultBufferSize
	}
	backend := rn.Backend
	if backend == BackendAuto {
		backend = defaultBackend
	}
	if backend != BackendEpoll {
		err := rn.startURing(bufferCount)
		switch {
		case err == nil:
			rn.backend = BackendIOUring
			return rn.finishStart()
		case backend == BackendAuto && ioURingUnavailable(err):
			rn.log.Warn("uringnet: io_uring unavailable, falling back to epoll (zero-copy send, recv bundles and "+
				"incremental buffers are disabled)", "ring", rn.index, "err", err)
		default:
			return err
		}
	}
	l, err := newEpollLoop(bufferCount, rn.BufferSize)
	if err != nil {
		return fmt.Errorf("%w: epoll: %v", errors.ErrRingSetup, err)
	}
	rn.loop, rn.br = l, l.BufferRing()
	rn.backend = BackendEpoll
	rn.maxHeld = bufferCount / 2
	rn.fixedIdx = -1
	rn.zcOff = true // no zero-copy send on epoll
	return rn.finishStart()
}

// startURing creates the io_uring event loop, falling back where features are unsupported.
func (rn *URingNet) startURing(bufferCount int) error {
	if err := checkKernel(); err != nil {
		return err
	}
	cfg := iouring.LoopConfig{
		Entries: rn.entries,
		BufferRings: []iouring.BufferRingConfig{{Count: bufferCount, Size: rn.BufferSize, Fixed: rn.ZeroCopyThreshold > 0,
			Incremental: rn.IncrementalBuffers}},
		SpinTime:      rn.SpinTime,
		SpinFixed:     rn.SpinFixed,
		SpinBusyBatch: rn.SpinBusyBatch,
		SpinIdleBatch: rn.SpinIdleBatch,
	}
	l, err := newURingLoop(cfg)
	if err != nil && cfg.BufferRings[0].Incremental && !unavailable(err) {
		// No incremental buffer rings (before 6.12): use a normal buffer ring
		rn.log.Info("uringnet: incremental buffers not supported, using a normal buffer ring", "ring", rn.index, "err", err)
		cfg.BufferRings[0].Incremental = false
		l, err = newURingLoop(cfg)
	}
	if err != nil && cfg.BufferRings[0].Fixed && !unavailable(err) {
		// Registering fixed buffers failed (usually RLIMIT_MEMLOCK): continue without; SEND_ZC still works but pins pages per send
		rn.log.Info("uringnet: cannot register receive buffers as fixed buffers (RLIMIT_MEMLOCK?), zero-copy sends will pin pages per send",
			"ring", rn.index, "err", err)
		cfg.BufferRings[0].Fixed = false
		l, err = newURingLoop(cfg)
	}
	if err != nil {
		return setupError(err)
	}
	rn.loop, rn.br = l, l.BufferRing()
	rn.maxHeld = bufferCount / 2
	if cfg.BufferRings[0].Incremental {
		rn.inc = true
		rn.incRefs = make([]int32, bufferCount)
		rn.incDone = make([]bool, bufferCount)
		// A referenced buffer can still receive (it is not blocked until full), so the limit can be much looser than
		// in normal mode: allowing all but one buffer to be held gave the fewest copies and highest throughput
		rn.maxHeld = bufferCount - 1
	}
	rn.bundle = !rn.inc && l.Features()&iouring_syscall.IORING_FEAT_RECVSEND_BUNDLE != 0 &&
		(rn.RecvBundle == BundleOn || (rn.RecvBundle == BundleAuto && bundleSafeKernel()))
	rn.fixedIdx = rn.br.FixedIndex()
	return nil
}

// finishStart is the setup shared by both backends.
func (rn *URingNet) finishStart() error {
	rn.conns = make(map[int]*conn)
	rn.maxWrite = rn.MaxWriteBuffer
	if rn.maxWrite == 0 {
		rn.maxWrite = DefaultMaxWriteBuffer
	}
	rn.nopH = func(int32, uint32) {}
	if rn.packet {
		rn.family = unix.AF_INET6
		if sa, err := unix.Getsockname(rn.SocketFd); err == nil {
			if _, ok := sa.(*unix.SockaddrInet4); ok {
				rn.family = unix.AF_INET
			}
		}
	}
	rn.setupTimeouts()
	return nil
}

// Connections returns the number of open connections on this ring.
func (rn *URingNet) Connections() int64 { return rn.connCount.Load() }

// Post runs fn on the ring's event loop thread; it is safe to call from any goroutine.
func (rn *URingNet) Post(fn func()) error {
	if rn.loop == nil {
		return errors.ErrRingSetup
	}
	return rn.loop.Post(fn)
}

// ---- loop thread ----

// Backoff after a failed accept: starts at acceptBackoffMin and doubles up to acceptBackoffMax.
const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
)

func (rn *URingNet) armAccept() {
	if rn.stopping || rn.draining || rn.SocketFd < 0 {
		return
	}
	if rn.packet {
		rn.armRecvMsg()
		return
	}
	rn.acceptArms++
	rn.loop.AcceptMultishot(rn.SocketFd, rn.onAccept)
}

func (rn *URingNet) onAccept(res int32, flags uint32) {
	if res >= 0 {
		rn.accepted++
		rn.acceptBackoff = 0
		if rn.stopping || rn.draining {
			unix.Close(int(res))
		} else {
			rn.openConn(int(res), nil, rn.tlsServer, false, nil)
		}
	}
	if flags&iouring_syscall.IORING_CQE_F_MORE != 0 || rn.stopping || rn.draining {
		return
	}
	// The multishot accept has ended
	if res >= 0 {
		rn.armAccept()
		return
	}
	rn.acceptErrs++
	// It ended with an error (e.g. EMFILE/ENFILE: out of fds). Re-arming at once usually fails again at once and
	// spins, so wait, doubling the wait on repeated failures; pending connections stay in the backlog until then.
	if rn.acceptBackoff == 0 {
		rn.acceptBackoff = acceptBackoffMin
	} else if rn.acceptBackoff *= 2; rn.acceptBackoff > acceptBackoffMax {
		rn.acceptBackoff = acceptBackoffMax
	}
	rn.log.Warn("uringnet: accept failed, retrying", "ring", rn.index, "err", syscall.Errno(-res), "retry_in", rn.acceptBackoff)
	rn.after(rn.acceptBackoff, rn.armAccept)
}

// openConn creates a connection for a connected fd (accepted or dialed) and calls OnOpen; ctx is set before OnOpen.
//
// With tlsCfg the connection first does a TLS handshake (as a client if client is set) and OnOpen runs
// once it has succeeded. onReady (may be nil) receives the connection after OnOpen, or the handshake error.
func (rn *URingNet) openConn(fd int, ctx interface{}, tlsCfg *tls.Config, client bool, onReady func(*conn, error)) *conn {
	c := newConn(rn, fd)
	c.api.ctx = ctx
	rn.conns[fd] = c
	rn.connCount.Add(1)

	if tlsCfg != nil {
		c.startTLS(tlsCfg, client, onReady)
		c.armRecv() // the handshake needs the peer's data
		return c
	}
	c.callOpen()
	if !c.closed && !c.paused {
		c.armRecv()
	}
	if onReady != nil {
		onReady(c, nil) // OnOpen may have closed the connection already; writes then return ErrConnClosed
	}
	return c
}

// callOpen calls OnOpen and sends what it wrote.
func (c *conn) callOpen() {
	c.opened = true
	c.batching = true
	c.rn.h.open(c)
	c.batching = false
	c.flush()
	c.checkBackpressure()
}

// shutdownLoop closes all connections; runs on the loop thread.
func (rn *URingNet) shutdownLoop() {
	rn.stopping = true
	// shutdown on the listening socket stops listening and fails pending accepts at once. close alone is not enough:
	// the multishot accept holds a file reference and the ring is released asynchronously, so the port would stay open for a while.
	if rn.SocketFd >= 0 {
		unix.Shutdown(rn.SocketFd, unix.SHUT_RD)
	}
	for _, c := range rn.conns {
		if c.err == nil {
			c.err = ErrServerClosed
		}
		c.free()
	}
}

// drain starts a graceful shutdown (loop thread): stop accepting and close each connection once it has sent what was
// written. Connections that make no progress are left to WriteTimeout or the final ShutDown.
func (rn *URingNet) drain() {
	if rn.draining || rn.stopping {
		return
	}
	rn.draining = true
	if rn.SocketFd >= 0 {
		unix.Shutdown(rn.SocketFd, unix.SHUT_RD)
	}
	for _, c := range rn.conns {
		if c.closed {
			continue
		}
		if c.err == nil {
			c.err = ErrServerClosed
		}
		c.closeGracefully()
	}
}

// ShutDown closes this ring: all connections and the event loop. Must not be called from a callback.
func (rn *URingNet) ShutDown() {
	rn.shutdownOnce.Do(func() {
		if rn.loop != nil {
			done := make(chan struct{})
			if rn.loop.Post(func() { rn.shutdownLoop(); close(done) }) == nil {
				select {
				case <-done:
				case <-rn.loop.Done():
				}
			}
			rn.loop.Close()
		}
		if rn.ownsSocket {
			unix.Close(rn.SocketFd)
		}
		if rn.h != nil {
			rn.h.shutdown(rn)
		}
	})
}

// checkKernel checks for Linux 6.0 or later (multishot recv and provided buffer rings).
func checkKernel() error {
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return nil
	}
	release := unix.ByteSliceToString(u.Release[:])
	var major, minor int
	if n, _ := fmt.Sscanf(release, "%d.%d", &major, &minor); n == 2 && major < 6 {
		return fmt.Errorf("%w: running %s", ErrKernelTooOld, release)
	}
	return nil
}

// unavailable reports whether an io_uring_setup error means io_uring cannot be used at all (rather than a feature being unsupported).
func unavailable(err error) bool {
	return stderrors.Is(err, syscall.EPERM) || stderrors.Is(err, syscall.ENOSYS) || stderrors.Is(err, syscall.EACCES)
}

// setupError adds the likely cause and a remedy to an event loop setup error.
func setupError(err error) error {
	switch {
	case unavailable(err):
		return fmt.Errorf("%w: %w: %v (blocked by seccomp? Docker needs --security-opt seccomp=unconfined or a profile "+
			"allowing io_uring_setup/io_uring_enter/io_uring_register; also check sysctl kernel.io_uring_disabled)",
			errors.ErrRingSetup, ErrIOUringUnavailable, err)
	case stderrors.Is(err, syscall.ENOMEM):
		return fmt.Errorf("%w: %v (out of locked memory? io_uring rings count against RLIMIT_MEMLOCK per user, "+
			"about 70KB per ring at the default SQ size of 512: raise ulimit -l / LimitMEMLOCK, or use fewer rings "+
			"(WithRings) or a smaller SQ (WithSQSize); with zero-copy the receive buffers are locked too)",
			errors.ErrRingSetup, err)
	}
	return fmt.Errorf("%w: %v", errors.ErrRingSetup, err)
}

func sockaddrToNetAddr(sa unix.Sockaddr) net.Addr {
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		return &net.TCPAddr{IP: append(net.IP(nil), a.Addr[:]...), Port: a.Port}
	case *unix.SockaddrInet6:
		addr := &net.TCPAddr{IP: append(net.IP(nil), a.Addr[:]...), Port: a.Port}
		if a.ZoneId != 0 {
			if ifi, err := net.InterfaceByIndex(int(a.ZoneId)); err == nil {
				addr.Zone = ifi.Name
			}
		}
		return addr
	case *unix.SockaddrUnix:
		return &net.UnixAddr{Name: a.Name, Net: "unix"}
	}
	return nil
}
