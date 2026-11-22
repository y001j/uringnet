//go:build linux
// +build linux

package uringnet

import (
	"context"
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

// DefaultBufferSize 是 provided buffer ring 中单个接收缓冲区的默认大小
const DefaultBufferSize = 16384

// DefaultMaxWriteBuffer 是每个连接发送队列的默认上限，见 URingNet.MaxWriteBuffer
const DefaultMaxWriteBuffer = 4 << 20

var (
	// ErrKernelTooOld is returned when the running kernel is older than Linux 6.0.
	ErrKernelTooOld = stderrors.New("uringnet: Linux 6.0 or later is required")
	// ErrIOUringUnavailable is returned when io_uring cannot be used: it is disabled by a
	// seccomp profile (Docker, Kubernetes RuntimeDefault), by kernel.io_uring_disabled, or
	// not compiled into the kernel.
	ErrIOUringUnavailable = stderrors.New("uringnet: io_uring is not available")
)

// discardLogger 是默认的日志：什么也不输出
var discardLogger = slog.New(discardHandler{})

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }

// URingNet 是一个 io_uring 事件循环实例。
//
// 每个 URingNet 独占一个 iouring.Loop（一个 io_uring + 一个锁定的 OS 线程），
// 所有回调（OnOpen/OnTraffic/OnWritten/OnClose）都在该线程中执行，不能阻塞。
type URingNet struct {
	Addr     string                // 监听地址
	Type     socket.NetAddressType // 连接类型
	SocketFd int                   // 监听 socket fd
	Handler  EventHandler          // 事件回调

	// 以下配置需在 SetLoops 之前设置

	// BufferSize 为单个接收缓冲区的字节数，默认 DefaultBufferSize
	BufferSize int

	// SpinTime 为事件循环阻塞前的忙轮询时长（0 表示不轮询）。
	// 以 CPU 换取更低的延迟，在唤醒代价高的环境（虚拟机）和中低并发下收益明显，建议 50µs。
	// 默认自适应：满载（每轮积压大量完成事件）时自动停止轮询，以免降低吞吐。
	SpinTime time.Duration

	// SpinFixed 为 true 时不论负载总是轮询（关闭自适应）
	SpinFixed bool

	// 自适应轮询的阈值：每轮事件循环平均处理的完成事件数达到 SpinBusyBatch 时停止轮询，
	// 低于 SpinIdleBatch 时恢复。0 表示使用默认值（128 / 64）。调大则在更高负载下继续轮询
	// （更看重延迟），调小则更早停止（更看重吞吐和 CPU）。
	SpinBusyBatch int
	SpinIdleBatch int

	// ZeroCopyThreshold 大于 0 时开启零拷贝发送（IORING_OP_SEND_ZC，Linux 6.0+）：
	// 不小于该字节数的原地发送（WriteBuf 指向接收缓冲区）和 WriteNoCopy 数据使用 SEND_ZC，
	// 内核直接从这块内存发送，不再拷贝到 socket 缓冲区。接收缓冲区同时注册为固定缓冲区
	// 零拷贝 pin 住的页计入 RLIMIT_MEMLOCK（ulimit -l）：注册失败时退回不注册，
	// SEND_ZC 返回 ENOMEM 时本实例自动关闭零拷贝、改用普通 send。
	// 零拷贝一般在 10KB 以上才划算；发往本机（回环）的数据内核总会拷贝，没有收益。
	ZeroCopyThreshold int

	// RecvBundle 控制 recv bundle（一次接收连续填充多个缓冲区，socket 中积压数据多时显著减少
	// 完成事件与回调次数）。默认 BundleAuto：仅在 Linux 6.16+ 上启用。6.10–6.15 的内核
	// 存在会使接收数据出现空洞的 bug（上游修复 3a08988123c8、26ec15e4b0c1、178b8ff66ff8，
	// 可能已回移到部分稳定版本），BundleOn 强制启用，仅用于测试或确认内核已包含修复的场景。
	RecvBundle BundleMode

	// IncrementalBuffers（实验性）使用增量消费的 buffer ring（Linux 6.12+）：少量大缓冲区被多次接收
	// 连续填充，一个连接可以原地发送多段数据而不拷贝。建议配合较大的 BufferSize（如 64KB）和较少的
	// 缓冲区数量。启用后不使用 recv bundle；内核不支持时自动退回普通模式。
	IncrementalBuffers bool

	// MaxWriteBuffer 为每个连接发送队列中待发送字节数的上限（背压）。超过时暂停接收该连接的数据
	// （不再调用 OnTraffic），队列降到一半以下时恢复。这样不读数据的对端不会让服务端无限缓存：
	// 接收暂停后对端的发送也会被 TCP 流控挡住。0 表示使用 DefaultMaxWriteBuffer，负数表示不限制。
	// 单次回调写入的数据不会被拒绝，所以队列可能短暂超过上限。
	MaxWriteBuffer int

	// IdleTimeout 大于 0 时，连接在这段时间内既没有收到也没有发出数据就被关闭。
	IdleTimeout time.Duration

	// WriteTimeout 大于 0 时，发送队列中有数据、但这段时间内发送没有任何进展（对端不读取）就关闭连接。
	// 与 MaxWriteBuffer 配合，可以清理只发不读或已经失联的对端。
	WriteTimeout time.Duration

	// Logger 接收运行中的异常和回退（accept 失败、零拷贝回退等），nil 表示不输出
	Logger *slog.Logger

	// Backend 选择事件循环的实现，默认 BackendAuto（io_uring 不可用时退回 epoll）
	Backend Backend

	index      int
	entries    uint32
	ownsSocket bool // SocketFd 是否由本实例独占（ReusePort 时每个实例一个监听 socket）

	h        connHandler // 回调：旧 API 时包装 Handler，新 API 时包装 Server 的 Handler
	log      *slog.Logger
	loop     eventLoop
	br       bufRing
	backend  Backend // 实际使用的后端
	ringloop *Ringloop

	// 以下字段仅在 Loop 线程中访问
	conns       map[int]*conn
	held        int     // 被发送队列占用的接收缓冲区数量
	maxHeld     int     // 原地发送最多占用的接收缓冲区数量（缓冲区总数的一半）
	starved     []*conn // 因缓冲区耗尽（-ENOBUFS）而暂停接收的连接
	stopping    bool
	draining    bool // 优雅关闭中：不再接受新连接，现有连接发完数据后关闭
	noDelay     bool // 拨出的 TCP 连接设置 TCP_NODELAY
	fixedIdx    int  // 接收缓冲区的固定缓冲区下标，-1 表示未注册
	zcOff       bool // 内核不支持零拷贝时自动关闭
	zcSends     int  // SEND_ZC 次数（测试用）
	bundle      bool // 是否使用 recv bundle
	bundleBufs  []iouring.BundleBuf
	inc         bool    // 是否使用增量消费的 buffer ring
	incRefs     []int32 // 增量消费：缓冲区被原地发送的片段引用数
	incDone     []bool  // 增量消费：内核已用完该缓冲区
	incPinned   int     // 增量消费：有片段引用的缓冲区数量
	gatherSends int     // sendmsg 聚集发送次数（测试用）
	maxWrite    int     // 生效的 MaxWriteBuffer，<= 0 表示不限制
	pauses      int     // 因背压暂停接收的次数（测试用）
	nopH        iouring.Handler

	// 超时检查：每 tickEvery 一个节拍，连接记录最近一次活动的节拍号（粗粒度时钟，不读系统时间）
	tick                  uint64
	tickEvery             time.Duration
	idleTicks, writeTicks uint64 // 超过这么多个节拍没有活动/发送进展就关闭；0 表示不检查

	acceptBackoff time.Duration // accept 出错后重新提交前的等待时间，成功 accept 后清零
	acceptArms    int           // 提交 accept 的次数（测试用）
	timeouts      int           // 因超时关闭的连接数（测试用）

	// 运行指标（Server.Stats），只在 Loop 线程中更新
	accepted, acceptErrs    uint64
	bytesIn, bytesOut       uint64
	enobufs                 uint64 // multishot recv 因缓冲区耗尽（-ENOBUFS）而终止的次数
	idleCloses, writeCloses uint64
	dialed                  uint64 // 拨出并建立的连接数

	// UDP（ListenPacket）
	packet                bool // 数据包模式：没有连接，multishot recvmsg 收数据报
	ph                    PacketHandler
	pc                    PacketConn
	recvMsg               unix.Msghdr // multishot recvmsg 的消息头（只用到地址长度）
	family                int         // socket 的协议族（AF_INET / AF_INET6）
	packetsIn, packetsOut uint64
	truncated, sendErrs   uint64

	connCount atomic.Int64

	shutdownOnce sync.Once
}

// UserData 是一个连接在回调中的视图，每个连接一个，在连接的整个生命周期内复用。
//
// Deprecated: use Handler, which receives a *Conn. UserData will be removed in a later release.
type UserData struct {
	Fd int32

	// Buffer 为本次 OnTraffic 收到的数据，只在 OnTraffic 执行期间有效，
	// 之后其内存会被归还给内核复用；需要保留时请拷贝。
	Buffer  []byte
	BufSize int32

	// WriteBuf 为要发送给对端的数据，在 OnOpen/OnTraffic 中设置并返回 Echo/Write/EchoAndClose。
	// 框架会拷贝 WriteBuf 的内容，回调返回后即可复用（框架会把它重置为长度 0）。
	WriteBuf []byte

	// Context 为用户自定义的连接上下文
	Context interface{}

	c *conn
}

// WriteNoCopy 把 b 加入发送队列而不拷贝，只能在回调中（事件循环线程）调用。
// 内核不再使用 b 后（发送完成；零拷贝时为通知到达后）调用 release（可为 nil），在此之前不能修改 b。
// 开启 ZeroCopyThreshold 且 b 足够大时使用 SEND_ZC 零拷贝发送。
// 它会立即入队，因此先于本次回调返回后才处理的 WriteBuf 发出。连接已关闭时立即调用 release 并返回错误。
func (d *UserData) WriteNoCopy(b []byte, release func()) error {
	return d.c.writeNoCopy(b, release)
}

// RemoteAddr 返回对端地址
func (d *UserData) RemoteAddr() net.Addr {
	sa, err := unix.Getpeername(int(d.Fd))
	if err != nil {
		return nil
	}
	return sockaddrToNetAddr(sa)
}

// LocalAddr 返回本端地址
func (d *UserData) LocalAddr() net.Addr {
	sa, err := unix.Getsockname(int(d.Fd))
	if err != nil {
		return nil
	}
	return sockaddrToNetAddr(sa)
}

// BundleMode 见 URingNet.RecvBundle
type BundleMode int

const (
	BundleAuto BundleMode = iota // Linux 6.16+ 且内核支持时启用
	BundleOn                     // 内核支持时启用（不检查版本）
	BundleOff                    // 不使用
)

// bundleSafeKernel 判断内核是否包含 recv bundle 的数据正确性修复（主线 6.16）
func bundleSafeKernel() bool {
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return false
	}
	var major, minor int
	fmt.Sscanf(unix.ByteSliceToString(u.Release[:]), "%d.%d", &major, &minor)
	return major > 6 || (major == 6 && minor >= 16)
}

// NetAddress 描述监听地址
//
// Deprecated: Listen takes the network and address directly.
type NetAddress struct {
	AddrType socket.NetAddressType
	Address  string
}

// New 创建单个 URingNet 实例，Handler 需在 SetLoops 之前设置。
//
// size 为 SQ 大小（内核会向上取整为 2 的幂）。sqpoll 已不再使用：
// 事件循环改为 SINGLE_ISSUER/DEFER_TASKRUN 模式，每轮只有一次 io_uring_enter，
// 不需要额外的 SQPOLL 内核线程空转。
//
// Deprecated: use Listen.
func New(addr NetAddress, size uint, sqpoll bool, options socket.SocketOptions) (*URingNet, error) {
	rings, err := NewMany(addr, size, sqpoll, 1, options, nil)
	if err != nil {
		return nil, err
	}
	return rings[0], nil
}

// NewMany 创建 num 个 URingNet 实例，之后需调用 SetLoops 和 Ringloop.RunMany 启动。
//
//	@param size   SQ 大小，内核会向上取整为 2 的幂
//	@param sqpoll 已废弃，保留以兼容旧接口
//	@param num    实例（事件循环）数量
//
// options.ReusePort 为 true 时每个实例创建自己的监听 socket，由内核在它们之间均衡新连接；
// 否则所有实例共享一个监听 socket。
// 目前只支持面向流的协议（tcp/tcp4/tcp6/unix）。
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
				// 端口为 0 时各个监听 socket 会绑到不同的随机端口：其余的改用第一个实际得到的端口
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

// boundAddress 在 address 的端口为 0 时返回带上 fd 实际端口的地址，否则原样返回
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
	// io_uring 负责异步等待，fd 使用阻塞模式：对 O_NONBLOCK 的 fd，部分内核会直接返回 -EAGAIN
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("%w: %v", errors.ErrSocketCreation, err)
	}
	return fd, nil
}

// start 创建事件循环，在 SetLoops 中调用
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
	rn.zcOff = true // epoll 没有零拷贝发送
	return rn.finishStart()
}

// startURing 创建 io_uring 事件循环，按需退回不支持的功能
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
		// 内核不支持增量消费（6.12 之前），退回普通 buffer ring
		rn.log.Info("uringnet: incremental buffers not supported, using a normal buffer ring", "ring", rn.index, "err", err)
		cfg.BufferRings[0].Incremental = false
		l, err = newURingLoop(cfg)
	}
	if err != nil && cfg.BufferRings[0].Fixed && !unavailable(err) {
		// 注册固定缓冲区失败（通常是 RLIMIT_MEMLOCK 不足），退回不注册：SEND_ZC 仍可用，只是逐次 pin 页
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
		// 被引用的缓冲区仍能继续接收（写满前不会挡住接收），上限可以比普通模式宽松得多：
		// 实测允许占住除 1 个外的全部缓冲区时用户态拷贝最少、吞吐最高
		rn.maxHeld = bufferCount - 1
	}
	rn.bundle = !rn.inc && l.Features()&iouring_syscall.IORING_FEAT_RECVSEND_BUNDLE != 0 &&
		(rn.RecvBundle == BundleOn || (rn.RecvBundle == BundleAuto && bundleSafeKernel()))
	rn.fixedIdx = rn.br.FixedIndex()
	return nil
}

// finishStart 是两种后端共同的初始化
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

// Connections 返回当前实例上的活跃连接数
func (rn *URingNet) Connections() int64 { return rn.connCount.Load() }

// Post 把 fn 投递到该实例的事件循环线程中执行，可在任意 goroutine 中调用
func (rn *URingNet) Post(fn func()) error {
	if rn.loop == nil {
		return errors.ErrRingSetup
	}
	return rn.loop.Post(fn)
}

// ---- Loop 线程 ----

// accept 出错后的退避时间：从 acceptBackoffMin 开始每次翻倍，最多 acceptBackoffMax
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
			rn.openConn(int(res), nil)
		}
	}
	if flags&iouring_syscall.IORING_CQE_F_MORE != 0 || rn.stopping || rn.draining {
		return
	}
	// multishot accept 已终止
	if res >= 0 {
		rn.armAccept()
		return
	}
	rn.acceptErrs++
	// 出错终止（如 EMFILE/ENFILE：fd 用完）。立即重新提交通常会马上再次失败，形成空转，
	// 所以等一段时间再提交，连续失败时等待时间翻倍；连接仍留在监听队列中，恢复后会被 accept。
	if rn.acceptBackoff == 0 {
		rn.acceptBackoff = acceptBackoffMin
	} else if rn.acceptBackoff *= 2; rn.acceptBackoff > acceptBackoffMax {
		rn.acceptBackoff = acceptBackoffMax
	}
	rn.log.Warn("uringnet: accept failed, retrying", "ring", rn.index, "err", syscall.Errno(-res), "retry_in", rn.acceptBackoff)
	rn.after(rn.acceptBackoff, rn.armAccept)
}

// openConn 为已连接的 fd（accept 或 connect 得到）创建连接并调用 OnOpen；ctx 在 OnOpen 之前设置
func (rn *URingNet) openConn(fd int, ctx interface{}) *conn {
	c := newConn(rn, fd)
	c.api.ctx = ctx
	rn.conns[fd] = c
	rn.connCount.Add(1)

	c.batching = true
	rn.h.open(c)
	c.batching = false
	c.flush()
	c.checkBackpressure()
	if !c.closed && !c.paused {
		c.armRecv()
	}
	return c
}

// shutdownLoop 在 Loop 线程中关闭所有连接
func (rn *URingNet) shutdownLoop() {
	rn.stopping = true
	// 对监听 socket 执行 shutdown 立即停止监听并使在途 accept 失败。仅 close 不够：
	// io_uring 中的 multishot accept 持有文件引用，ring 的释放是异步的，端口会短暂保持监听。
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

// drain 开始优雅关闭（Loop 线程）：停止接受新连接，每个连接把已写入的数据发完后关闭。
// 发送一直没有进展的连接由 WriteTimeout 或最终的 ShutDown 处理。
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
		c.closeAfterFlush()
	}
}

// ShutDown 关闭该实例：关闭所有连接、停止事件循环。不能在回调中调用。
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

// checkKernel 检查内核版本不低于 6.0（multishot recv 与 provided buffer ring）
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

// unavailable 判断 io_uring_setup 的错误是否表示 io_uring 不可用（而不是某个功能不支持）
func unavailable(err error) bool {
	return stderrors.Is(err, syscall.EPERM) || stderrors.Is(err, syscall.ENOSYS) || stderrors.Is(err, syscall.EACCES)
}

// setupError 给创建事件循环失败的错误加上原因和解决办法
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
