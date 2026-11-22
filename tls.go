//go:build linux
// +build linux

package uringnet

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// tlsHandshakeTimeout bounds the TLS handshake of a connection.
const tlsHandshakeTimeout = 10 * time.Second

// tlsState runs crypto/tls for one connection, on top of an in-memory transport that the event loop
// feeds with the ciphertext it receives.
//
// crypto/tls cannot pause a handshake halfway, so the handshake runs on a goroutine of its own with a
// blocking transport. Once it is done the goroutine exits and the loop thread decrypts by itself: the
// transport switches to non-blocking and returns a temporary net.Error when a record is incomplete,
// which crypto/tls does not treat as fatal after the handshake (it keeps the partial record and
// continues on the next Read). So in the steady state there is no goroutine switch: received records
// are decrypted and passed to OnTraffic on the loop thread, and writes are encrypted there into the
// normal send queue, where backpressure and timeouts apply unchanged.
type tlsState struct {
	c       *conn
	tc      *tls.Conn
	tr      *tlsTransport
	ready   bool               // handshake done and OnOpen called; loop thread only
	onReady func(*conn, error) // dial: reports the result after the handshake; loop thread only
}

// startTLS sets up TLS on a new connection and starts the handshake goroutine.
func (c *conn) startTLS(cfg *tls.Config, client bool, onReady func(*conn, error)) {
	tr := &tlsTransport{rn: c.rn}
	tr.cond = sync.NewCond(&tr.mu)
	if sa, err := unix.Getsockname(c.fd); err == nil {
		tr.local = sockaddrToNetAddr(sa)
	}
	if sa, err := unix.Getpeername(c.fd); err == nil {
		tr.remote = sockaddrToNetAddr(sa)
	}
	ts := &tlsState{c: c, tr: tr, onReady: onReady}
	tr.flush = func() { ts.flushOut() }
	if client {
		ts.tc = tls.Client(tr, cfg)
	} else {
		ts.tc = tls.Server(tr, cfg)
	}
	c.tls = ts
	go ts.run()
}

// run is the connection's handshake goroutine.
func (ts *tlsState) run() {
	ctx, cancel := context.WithTimeout(context.Background(), tlsHandshakeTimeout)
	err := ts.tc.HandshakeContext(ctx)
	cancel()
	ts.c.rn.Post(func() { ts.handshakeDone(err) })
}

// pump decrypts the records received so far and passes them to OnTraffic (loop thread, after the
// handshake). It stops when the next record is incomplete, the connection ends or the peer closed.
func (ts *tlsState) pump() {
	c := ts.c
	if c.rn.tlsBuf == nil {
		c.rn.tlsBuf = make([]byte, 64<<10) // shared by the ring's connections: data is valid during OnTraffic only
	}
	for !c.closed {
		n, err := ts.tc.Read(c.rn.tlsBuf)
		if n > 0 {
			c.deliverPlain(c.rn.tlsBuf[:n])
		}
		if err != nil {
			if err != errWouldBlock {
				ts.readDone(err)
			}
			return
		}
	}
}

// handshakeDone runs on the loop thread when the handshake finished or failed.
func (ts *tlsState) handshakeDone(err error) {
	c := ts.c
	onReady := ts.onReady
	ts.onReady = nil
	if err == nil && c.closed {
		err = ErrConnClosed
	}
	if err != nil {
		c.rn.tlsErrors++
		c.rn.log.Debug("uringnet: TLS handshake failed", "fd", c.fd, "err", err)
		c.fail(err) // OnOpen was never called, so OnClose is not either
		if onReady != nil {
			onReady(nil, err)
		}
		return
	}
	ts.ready = true
	ts.tr.setNonblocking()
	c.lastActive, c.lastSend = c.rn.tick, c.rn.tick
	c.callOpen()
	if onReady != nil {
		onReady(c, nil)
	}
	ts.pump() // data that arrived with or right after the handshake
}

// readDone runs on the loop thread when the decrypting goroutine stopped: the peer closed the
// connection (io.EOF), or an error such as a bad record.
func (ts *tlsState) readDone(err error) {
	c := ts.c
	if c.closed {
		return
	}
	if err == io.EOF {
		// All data before the peer's close_notify (or EOF) has been delivered: send what was
		// written, then close, as for a plain half-close
		if c.err == nil {
			c.err = io.EOF
		}
		c.closeGracefully()
		return
	}
	c.fail(err)
}

// write encrypts b on the loop thread and queues the ciphertext.
func (ts *tlsState) write(b []byte) {
	if len(b) == 0 {
		return
	}
	if _, err := ts.tc.Write(b); err != nil {
		ts.c.fail(err)
		return
	}
	ts.flushOut()
}

// closeNotify sends the TLS close_notify alert (loop thread).
func (ts *tlsState) closeNotify() {
	if ts.ready {
		ts.tc.CloseWrite()
		ts.flushOut()
	}
}

// flushOut moves the ciphertext written by tls.Conn into the send queue (loop thread).
func (ts *tlsState) flushOut() {
	tr := ts.tr
	c := ts.c
	// Give the transport a copy buffer the connection has finished sending, so encrypting the next
	// write does not allocate
	var next []byte
	if n := len(c.spare); n > 0 {
		next = c.spare[n-1][:0]
		c.spare = c.spare[:n-1]
	}
	tr.mu.Lock()
	out := tr.out
	if len(out) > 0 {
		tr.out = next
		next = nil
	}
	tr.flushQueued = false
	tr.mu.Unlock()
	if next != nil {
		c.spare = append(c.spare, next)
	}
	if len(out) > 0 && !ts.c.closed {
		ts.c.writeOwned(out) // the transport starts a new buffer, so this one can be queued without a copy
	}
}

// deliverPlain passes decrypted data to OnTraffic (loop thread). Inside onRecv the caller sends
// what was written; on its own (after the handshake) it sends it here.
func (c *conn) deliverPlain(data []byte) {
	if c.closed || c.closeAfter {
		return
	}
	outer := c.batching
	c.batching = true
	c.rn.h.traffic(c, data)
	c.batching = outer
	if !outer {
		c.flush()
		c.checkBackpressure()
	}
}

// errWouldBlock is returned by the transport after the handshake when no complete record is
// buffered. It is a temporary net.Error, which crypto/tls does not record as a fatal error.
var errWouldBlock error = wouldBlock{}

type wouldBlock struct{}

func (wouldBlock) Error() string   { return "uringnet: TLS record incomplete" }
func (wouldBlock) Timeout() bool   { return true }
func (wouldBlock) Temporary() bool { return true }

// tlsTransport is the in-memory net.Conn under tls.Conn. Read blocks the TLS goroutine until the
// event loop has fed ciphertext; Write collects ciphertext and asks the loop to send it.
type tlsTransport struct {
	rn          *URingNet
	mu          sync.Mutex
	cond        *sync.Cond
	in          []byte // ciphertext received and not yet read by tls.Conn
	inErr       error  // io.EOF after the peer closed, net.ErrClosed after Close
	out         []byte // ciphertext written by tls.Conn and not yet queued
	flushQueued bool
	nonblock    bool // after the handshake: Read returns errWouldBlock instead of waiting
	flush       func()
	local       net.Addr
	remote      net.Addr
}

// feed adds received ciphertext (loop thread).
func (t *tlsTransport) feed(b []byte) {
	t.mu.Lock()
	t.in = append(t.in, b...)
	t.mu.Unlock()
	t.cond.Signal()
}

// lend makes b readable without copying it; unlend must follow before b is reused (loop thread,
// after the handshake, when no other goroutine reads the transport).
func (t *tlsTransport) lend(b []byte) {
	t.mu.Lock()
	if len(t.in) == 0 {
		t.in = b
	} else {
		t.in = append(t.in, b...)
	}
	t.mu.Unlock()
}

// unlend copies whatever crypto/tls left unread, so the lent buffer can be reused.
func (t *tlsTransport) unlend() {
	t.mu.Lock()
	if len(t.in) > 0 {
		t.in = append([]byte(nil), t.in...)
	} else {
		t.in = nil
	}
	t.mu.Unlock()
}

func (t *tlsTransport) setNonblocking() {
	t.mu.Lock()
	t.nonblock = true
	t.mu.Unlock()
}

// end makes Read return err once the buffered data is consumed (loop thread or tls.Conn).
func (t *tlsTransport) end(err error) {
	t.mu.Lock()
	if t.inErr == nil {
		t.inErr = err
	}
	t.mu.Unlock()
	t.cond.Broadcast()
}

func (t *tlsTransport) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(t.in) == 0 && t.inErr == nil {
		if t.nonblock {
			return 0, errWouldBlock
		}
		t.cond.Wait()
	}
	if len(t.in) > 0 {
		n := copy(p, t.in)
		t.in = t.in[n:]
		if len(t.in) == 0 {
			t.in = nil
		}
		return n, nil
	}
	return 0, t.inErr
}

func (t *tlsTransport) Write(p []byte) (int, error) {
	t.mu.Lock()
	if t.inErr == net.ErrClosed {
		t.mu.Unlock()
		return 0, net.ErrClosed
	}
	t.out = append(t.out, p...)
	post := !t.flushQueued
	t.flushQueued = true
	t.mu.Unlock()
	if post {
		// From the TLS goroutine (handshake messages, alerts) the loop must send the data; writes on
		// the loop thread flush right away and make this a no-op
		t.rn.Post(t.flush)
	}
	return len(p), nil
}

func (t *tlsTransport) Close() error                     { t.end(net.ErrClosed); return nil }
func (t *tlsTransport) LocalAddr() net.Addr              { return t.local }
func (t *tlsTransport) RemoteAddr() net.Addr             { return t.remote }
func (t *tlsTransport) SetDeadline(time.Time) error      { return nil }
func (t *tlsTransport) SetReadDeadline(time.Time) error  { return nil }
func (t *tlsTransport) SetWriteDeadline(time.Time) error { return nil }
func (t *tlsTransport) String() string                   { return "uringnet TLS transport" }
