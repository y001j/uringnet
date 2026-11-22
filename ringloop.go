//go:build linux
// +build linux

package uringnet

import (
	"fmt"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Ringloop manages a set of URingNet event loops.
//
// Deprecated: use Server (Listen, Serve, Shutdown).
type Ringloop struct {
	RingNet   []*URingNet // the rings
	RingCount int32       // number of rings

	listenFds []int // shared listening sockets, closed by the Ringloop

	stop         chan struct{}
	shutdownOnce sync.Once
	done         chan struct{}
}

// SetLoops creates the event loop (io_uring and provided buffer ring, or epoll) of each URingNet.
//
//	@param bufferCount receive buffers per ring, rounded up to a power of 2 (max 32768);
//	                   the size of each is URingNet.BufferSize. Use at least as many as connections
//	                   active at once on a ring (otherwise -ENOBUFS is frequent), otherwise as few as
//	                   possible: the kernel uses them in FIFO order, so fewer stay warmer in the cache.
//
// Deprecated: use Listen with WithBuffers.
func SetLoops(urings []*URingNet, bufferCount int) (*Ringloop, error) {
	size := len(urings)
	if size == 0 {
		return nil, fmt.Errorf("no uring instances provided")
	}
	bufferCount = roundPow2(bufferCount)

	theloop := &Ringloop{
		RingNet:   urings,
		RingCount: int32(size),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	shared := map[int]bool{}
	for i, rn := range urings {
		if err := rn.start(bufferCount); err != nil {
			for _, started := range urings[:i] {
				started.loop.Close()
			}
			return nil, fmt.Errorf("failed to set up ring %d: %w", i, err)
		}
		rn.ringloop = theloop
		if !rn.ownsSocket && rn.SocketFd >= 0 && !shared[rn.SocketFd] {
			shared[rn.SocketFd] = true
			theloop.listenFds = append(theloop.listenFds, rn.SocketFd)
		}
	}
	return theloop, nil
}

func roundPow2(n int) int {
	if n <= 0 {
		return 1024
	}
	if n > 32768 {
		return 32768
	}
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// RunMany starts accepting connections on all rings and returns at once; Wait blocks until shutdown.
func (loop *Ringloop) RunMany() {
	for _, rn := range loop.RingNet {
		rn := rn
		rn.h.boot(rn)
		rn.loop.Post(rn.armAccept)
		rn.loop.Post(rn.startTimeouts)
	}
	loop.startTicker()
}

// RunMany2 is the same as RunMany, kept for compatibility.
func (loop *Ringloop) RunMany2() { loop.RunMany() }

// Wait blocks until Shutdown has completed.
func (loop *Ringloop) Wait() { <-loop.done }

// Shutdown closes all connections and event loops. It may be called from any goroutine (return the Shutdown action from callbacks).
func (loop *Ringloop) Shutdown() {
	loop.shutdownOnce.Do(func() {
		close(loop.stop)
		var wg sync.WaitGroup
		for _, rn := range loop.RingNet {
			wg.Add(1)
			go func(rn *URingNet) {
				defer wg.Done()
				rn.ShutDown()
			}(rn)
		}
		wg.Wait()
		for _, fd := range loop.listenFds {
			unix.Close(fd)
		}
		close(loop.done)
	})
}

// startTicker drives OnTick (with the first ring's Handler); OnTick runs on its own goroutine.
func (loop *Ringloop) startTicker() {
	h := loop.RingNet[0].Handler
	if h == nil { // the Handler API has no OnTick
		return
	}
	delay, action := h.OnTick()
	if action == Shutdown {
		go loop.Shutdown()
		return
	}
	if delay <= 0 {
		return
	}
	go func() {
		t := time.NewTimer(delay)
		defer t.Stop()
		for {
			select {
			case <-loop.stop:
				return
			case <-t.C:
			}
			d, act := h.OnTick()
			if act == Shutdown {
				go loop.Shutdown()
				return
			}
			if d <= 0 {
				return
			}
			t.Reset(d)
		}
	}()
}

// Action is an action that occurs after the completion of an event.
type Action int

const (
	// None indicates that no action should occur following an event.
	None Action = iota
	// Echo sends UserData.WriteBuf.
	//
	// Deprecated: write with Conn.Write and return None.
	Echo
	// Read does nothing; receiving is always active.
	//
	// Deprecated: return None.
	Read
	// EchoAndClose sends UserData.WriteBuf, then closes the connection.
	//
	// Deprecated: write with Conn.Write and return Close.
	EchoAndClose
	// Write sends UserData.WriteBuf.
	//
	// Deprecated: write with Conn.Write and return None.
	Write
	// Close closes the connection. With Handler, data already written is sent first.
	Close
	// Shutdown shuts the whole server down.
	Shutdown
)

type (
	// EventHandler represents the engine events' callbacks for the Run call.
	// Each event has an Action return value that is used manage the state
	// of the connection and engine.
	//
	// Deprecated: implement Handler and use Listen.
	EventHandler interface {
		// OnBoot fires when the engine is ready for accepting connections.
		// The parameter engine has information and various utilities.
		OnBoot(eng *URingNet) (action Action)

		// OnShutdown fires when the engine is being shut down, it is called right after
		// all event-loops and connections are closed.
		OnShutdown(eng *URingNet)

		// OnOpen fires when a new connection has been opened.
		// The Conn c has information about the connection such as its local and remote address.
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
		OnTraffic(data *UserData, eng *URingNet) (action Action)

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
	//
	// Deprecated: embed BuiltinHandler in a Handler.
	BuiltinEventEngine struct {
		ctx interface{}
	}
)

// OnBoot fires when the engine is ready for accepting connections.
// The parameter engine has information and various utilities.
func (es *BuiltinEventEngine) OnBoot(_ *URingNet) (action Action) {
	return
}

// OnShutdown fires when the engine is being shut down, it is called right after
// all event-loops and connections are closed.
func (es *BuiltinEventEngine) OnShutdown(_ *URingNet) {
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
func (es *BuiltinEventEngine) OnTraffic(_ *UserData, _ *URingNet) (action Action) {
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
