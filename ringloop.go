//go:build linux
// +build linux

package UringNet

import (
	"bytes"
	"fmt"
	"github.com/y001j/UringNet/uring"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
)

type Ringloop struct {
	socketFd int // listener
	//idx          int             // loop index in the engine loops list
	cache bytes.Buffer // temporary buffer for scattered bytes
	//engine       *engine         // engine in loop
	//poller       *netpoll.Poller // epoll or kqueue
	RingNet     []*URingNet       //io_uring instance used in the loop
	buffer      [][bufLength]byte // read packet buffer whose capacity is set by user, default value is 64KB
	RingCount   int32             // number of active connections in event-loop
	udpSockets  map[int]*conn     // client-side UDP socket map: fd -> conn
	connections sync.Map          // map[int]*conn // TCP connection map: fd -> conn
	//eventHandler EventHandler  // user eventHandler
}

// set the buffer length, default buffer length is 1024
const bufLength uint64 = 2048

// SetLoops
//
//	@Description: set the ringloop for the engine
//	@param urings
//	@return *Ringloop
func SetLoops(urings []*URingNet, bufferSize int) *Ringloop {
	size := len(urings)
	theloop := &Ringloop{}
	theloop.RingCount = int32(size)
	//theloop.connections = map[int]*conn{}
	theloop.RingNet = urings
	for i := 0; i < size; i++ {

		urings[i].ringloop = theloop
		theloop.RingNet[i] = urings[i]
		theloop.socketFd = urings[i].SocketFd

		//set buffer
		sqe2 := theloop.RingNet[i].ring.GetSQEntry()
		//Buffers := make([][]byte, 1024, 1024)
		//tempb := Init2DSlice(1024, 1024)
		urings[i].Autobuffer = make([][bufLength]byte, bufferSize)
		uring.ProvideBuf(sqe2, urings[i].Autobuffer, uint32(bufferSize), uint32(bufLength), uint16(i))
		data := makeUserData(provideBuffer)
		sqe2.SetUserData(data.id)
		theloop.RingNet[i].userDataList.Store(data.id, data)
		//theloop.RingNet[i].userDataMap[data.id] = data
		fmt.Println("Add Kernel buffer... for ring ", i)
		_, _ = theloop.RingNet[i].ring.Submit(1, &paraFlags)
		//theloop.RingNet[i].ringloop
	}
	return theloop
}

func (loop *Ringloop) GetBuffer() [][bufLength]byte {
	return loop.buffer
}

// Create a accept event  for the loop.
// the accept should be set every time when server is initiated.
func (ringNet *URingNet) EchoLoop() {

	sqe := ringNet.ring.GetSQEntry()
	data := makeUserData(accepted)
	var len uint32 = unix.SizeofSockaddrAny
	data.ClientSock = &syscall.RawSockaddrAny{}
	data.socklen = &len
	sqe.SetUserData(data.id)

	//sqe.SetAddr()
	//fmt.Println(sqe.UserData())
	ringNet.userDataList.Store(data.id, data)
	//ringNet.userDataMap[data.id] = data
	//set client address in data.client
	uring.Accept(sqe, uintptr(ringNet.SocketFd), nil, nil)
	_, err := ringNet.ring.Submit(0, &paraFlags)

	//fmt.Println("echo server running...")

	if err != nil {
		fmt.Printf("prep request error: %v\n", err)
		return
	}
}

func (loop *Ringloop) RunMany() {

	for i := 0; i < int(loop.RingCount); i++ {
		loop.RingNet[i].EchoLoop()
		go loop.RingNet[i].Run2(uint16(i))
	}
}

func (loop *Ringloop) RunMany2() {

	for i := 0; i < int(loop.RingCount); i++ {
		loop.RingNet[i].EchoLoop()
		go loop.RingNet[i].Run(uint16(i))
	}
}

//func (loop *Ringloop) RunManyAcceptor() {
//	for i := 0; i < int(loop.RingCount); i++ {
//		loop.RingNet[i].EchoLoop()
//		go loop.RingNet[i].Run(uint16(i))
//	}
//}

// Action is an action that occurs after the completion of an event.
type Action int

const (
	// None indicates that no action should occur following an event.
	None Action = iota
	Echo
	Read
	EchoAndClose // response then close
	Write
	Close //Close the connection.

	// Shutdown shutdowns the engine.
	Shutdown
)

// Reader is an interface that consists of a number of methods for reading that Conn must implement.
type Reader interface {
	// ================================== Non-concurrency-safe API's ==================================

	io.Reader
	io.WriterTo // must be non-blocking, otherwise it may block the event-loop.

	// Next returns a slice containing the next n bytes from the buffer,
	// advancing the buffer as if the bytes had been returned by Read.
	// If there are fewer than n bytes in the buffer, Next returns the entire buffer.
	// The error is ErrBufferFull if n is larger than b's buffer size.
	//
	// Note that the []byte buf returned by Next() is not allowed to be passed to a new goroutine,
	// as this []byte will be reused within event-loop.
	// If you have to use buf in a new goroutine, then you need to make a copy of buf and pass this copy
	// to that new goroutine.
	Next(n int) (buf []byte, err error)

	// Peek returns the next n bytes without advancing the reader. The bytes stop
	// being valid at the next read call. If Peek returns fewer than n bytes, it
	// also returns an error explaining why the read is short. The error is
	// ErrBufferFull if n is larger than b's buffer size.
	//
	// Note that the []byte buf returned by Peek() is not allowed to be passed to a new goroutine,
	// as this []byte will be reused within event-loop.
	// If you have to use buf in a new goroutine, then you need to make a copy of buf and pass this copy
	// to that new goroutine.
	Peek(n int) (buf []byte, err error)

	// Discard skips the next n bytes, returning the number of bytes discarded.
	//
	// If Discard skips fewer than n bytes, it also returns an error.
	// If 0 <= n <= b.Buffered(), Discard is guaranteed to succeed without
	// reading from the underlying io.Reader.
	Discard(n int) (discarded int, err error)

	// InboundBuffered returns the number of bytes that can be read from the current buffer.
	InboundBuffered() (n int)
}

// Writer is an interface that consists of a number of methods for writing that Conn must implement.
type Writer interface {
	// ================================== Non-concurrency-safe API's ==================================

	io.Writer
	io.ReaderFrom // must be non-blocking, otherwise it may block the event-loop.

	// Writev writes multiple byte slices to peer synchronously, you must call it in the current goroutine.
	Writev(bs [][]byte) (n int, err error)

	// Flush writes any buffered data to the underlying connection, you must call it in the current goroutine.
	Flush() (err error)

	// OutboundBuffered returns the number of bytes that can be read from the current buffer.
	OutboundBuffered() (n int)

	// ==================================== Concurrency-safe API's ====================================

	// AsyncWrite writes one byte slice to peer asynchronously, usually you would call it in individual goroutines
	// instead of the event-loop goroutines.
	AsyncWrite(buf []byte, callback AsyncCallback) (err error)

	// AsyncWritev writes multiple byte slices to peer asynchronously, usually you would call it in individual goroutines
	// instead of the event-loop goroutines.
	AsyncWritev(bs [][]byte, callback AsyncCallback) (err error)
}

// AsyncCallback is a callback which will be invoked after the asynchronous functions has finished executing.
//
// Note that the parameter gnet.Conn is already released under UDP protocol, thus it's not allowed to be accessed.
type AsyncCallback func(c Conn) error

// Socket is a set of functions which manipulate the underlying file descriptor of a connection.
type Socket interface {
	// Fd returns the underlying file descriptor.
	Fd() int

	// Dup returns a copy of the underlying file descriptor.
	// It is the caller's responsibility to close fd when finished.
	// Closing c does not affect fd, and closing fd does not affect c.
	//
	// The returned file descriptor is different from the
	// connection's. Attempting to change properties of the original
	// using this duplicate may or may not have the desired effect.
	Dup() (int, error)

	// SetReadBuffer sets the size of the operating system's
	// receive buffer associated with the connection.
	SetReadBuffer(bytes int) error

	// SetWriteBuffer sets the size of the operating system's
	// transmit buffer associated with the connection.
	SetWriteBuffer(bytes int) error

	// SetLinger sets the behavior of Close on a connection which still
	// has data waiting to be sent or to be acknowledged.
	//
	// If sec < 0 (the default), the operating system finishes sending the
	// data in the background.
	//
	// If sec == 0, the operating system discards any unsent or
	// unacknowledged data.
	//
	// If sec > 0, the data is sent in the background as with sec < 0. On
	// some operating systems after sec seconds have elapsed any remaining
	// unsent data may be discarded.
	SetLinger(sec int) error

	// SetKeepAlivePeriod tells operating system to send keep-alive messages on the connection
	// and sets period between TCP keep-alive probes.
	SetKeepAlivePeriod(d time.Duration) error

	// SetNoDelay controls whether the operating system should delay
	// packet transmission in hopes of sending fewer packets (Nagle's
	// algorithm).
	// The default is true (no delay), meaning that data is sent as soon as possible after a Write.
	SetNoDelay(noDelay bool) error
	// CloseRead() error
	// CloseWrite() error
}

// Conn is an interface of underlying connection.
type Conn interface {
	Reader
	Writer
	Socket

	// ================================== Non-concurrency-safe API's ==================================

	// Context returns a user-defined context.
	Context() (ctx interface{})

	// SetContext sets a user-defined context.
	SetContext(ctx interface{})

	// LocalAddr is the connection's local socket address.
	LocalAddr() (addr net.Addr)

	// RemoteAddr is the connection's remote peer address.
	RemoteAddr() (addr net.Addr)

	// SetDeadline implements net.Conn.
	SetDeadline(t time.Time) (err error)

	// SetReadDeadline implements net.Conn.
	SetReadDeadline(t time.Time) (err error)

	// SetWriteDeadline implements net.Conn.
	SetWriteDeadline(t time.Time) (err error)

	// ==================================== Concurrency-safe API's ====================================

	// Wake triggers a OnTraffic event for the connection.
	Wake(callback AsyncCallback) (err error)

	// Close closes the current connection, usually you don't need to pass a non-nil callback
	// because you should use OnClose() instead, the callback here is only for compatibility.
	Close(callback AsyncCallback) (err error)
}

type (
	// EventHandler represents the engine events' callbacks for the Run call.
	// Each event has an Action return value that is used manage the state
	// of the connection and engine.
	EventHandler interface {
		// OnBoot fires when the engine is ready for accepting connections.
		// The parameter engine has information and various utilities.
		OnBoot(eng URingNet) (action Action)

		// OnShutdown fires when the engine is being shut down, it is called right after
		// all event-loops and connections are closed.
		OnShutdown(eng URingNet)

		// OnOpen fires when a new connection has been opened.
		// The Conn c has information about the connection such as it's local and remote address.
		// The parameter out is the return value which is going to be sent back to the peer.
		// It is usually not recommended to send large amounts of data back to the peer in OnOpened.
		//
		// Note that the bytes returned by OnOpened will be sent back to the peer without being encoded.
		OnOpen(data *UserData) (out []byte, action Action)

		// OnClose fires when a connection has been closed.
		// The parameter err is the last known connection error.
		OnClose(data UserData) (action Action)

		// OnTraffic fires when a socket receives data from the peer.
		//
		// Note that the parameter packet returned from React() is not allowed to be passed to a new goroutine,
		// as this []byte will be reused within event-loop after React() returns.
		// If you have to use packet in a new goroutine, then you need to make a copy of buf and pass this copy
		// to that new goroutine.
		OnTraffic(data *UserData, eng URingNet) (action Action)

		// OnTick fires immediately after the engine starts and will fire again
		// following the duration specified by the delay return value.
		OnTick() (delay time.Duration, action Action)

		// OnWritten fires immediately after the Written/Response completed
		OnWritten(data UserData) (action Action)

		// Context returns a user-defined context.
		Context() (ctx interface{})

		// SetContext sets a user-defined context.
		SetContext(ctx interface{})
	}

	// BuiltinEventEngine is a built-in implementation of EventHandler which sets up each method with a default implementation,
	// you can compose it with your own implementation of EventHandler when you don't want to implement all methods
	// in EventHandler.
	BuiltinEventEngine struct {
		ctx interface{}
	}
)

// OnBoot fires when the engine is ready for accepting connections.
// The parameter engine has information and various utilities.
func (es *BuiltinEventEngine) OnBoot(_ URingNet) (action Action) {
	return
}

// OnShutdown fires when the engine is being shut down, it is called right after
// all event-loops and connections are closed.
func (es *BuiltinEventEngine) OnShutdown(_ URingNet) {
}

// OnOpen fires when a new connection has been opened.
// The parameter out is the return value which is going to be sent back to the peer.
func (es *BuiltinEventEngine) OnOpen(_ *UserData) (out []byte, action Action) {
	return
}

// OnClose fires when a connection has been closed.
// The parameter err is the last known connection error.
func (es *BuiltinEventEngine) OnClose(_ UserData) (action Action) {
	return
}

// OnTraffic fires when a local socket receives data from the peer.
func (es *BuiltinEventEngine) OnTraffic(_ *UserData, _ URingNet) (action Action) {
	return
}

// OnTick fires immediately after the engine starts and will fire again
// following the duration specified by the delay return value.
func (es *BuiltinEventEngine) OnTick() (delay time.Duration, action Action) {
	return
}

// OnWritten fires immediately after the Written/Response completed
func (es *BuiltinEventEngine) OnWritten(_ UserData) (action Action) {
	return
}

func (es *BuiltinEventEngine) Context() (ctx interface{}) {

	return es.ctx

}

func (es *BuiltinEventEngine) SetContext(ctx interface{}) {

	es.ctx = ctx

}
