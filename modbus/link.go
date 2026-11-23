//go:build linux
// +build linux

package modbus

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/y001j/uringnet"
)

// LinkConfig configures the master side of a connection.
type LinkConfig struct {
	// Framing is Modbus TCP (the default) or RTU over TCP.
	Framing Framing
	// Timeout is how long to wait for each response (default 1s).
	Timeout time.Duration
	// Retries is how many times a request is sent again after a timeout or, on a Client, after
	// the connection is lost while it is outstanding (default 0). Requests not sent yet simply wait
	// for the next connection. Exception responses are not retried.
	Retries int
	// MaxInflight is how many Modbus TCP requests may be outstanding at once on a connection
	// (default 1, which every device supports). RTU over TCP always sends one at a time.
	MaxInflight int
	// Drain is how long RTU over TCP waits after a timeout or a broadcast before sending the next
	// request (default 50ms). RTU frames carry no transaction ID: a response arriving after its
	// timeout is recognized as late if nothing is outstanding, or if it does not fit the outstanding
	// request (another unit, function, quantity or written value); a late response that fits
	// exactly would be taken as the answer. Set Drain to cover how late a device may still answer.
	Drain time.Duration
}

func (c *LinkConfig) setDefaults() {
	if c.Timeout <= 0 {
		c.Timeout = time.Second
	}
	if c.Retries < 0 {
		c.Retries = 0
	}
	if c.MaxInflight <= 0 || c.Framing == RTUOverTCP {
		c.MaxInflight = 1
	}
	if c.Drain <= 0 {
		c.Drain = 50 * time.Millisecond
	}
}

// LinkStats are a Link's counters.
type LinkStats struct {
	Requests  uint64 // request frames sent, retries included
	Responses uint64 // responses matched to a request
	Timeouts  uint64 // responses that did not arrive in time
	Retries   uint64 // requests sent again
	Dropped   uint64 // frames dropped: no matching request, wrong CRC, or unparseable bytes
}

// Link is a Modbus master on one connection to a device or gateway: an outgoing connection made
// by a Client, or a device's incoming connection accepted by a DTUServer. Its methods are safe for
// concurrent use; requests are sent in the order they are made.
//
// A Client's Link reconnects by itself; requests made while it is disconnected wait for the
// connection (bounded by their context). A DTUServer's Link ends with its connection.
type Link struct {
	cfg       LinkConfig
	id        string
	heartbeat []byte  // DTU heartbeat packet, discarded between frames
	client    *Client // nil for a DTU link
	down      chan struct{}

	mu         sync.Mutex
	conn       *uringnet.Conn // nil while disconnected
	closed     bool
	queue      []*call
	inflight   map[uint16]*call // by transaction ID; RTU uses 0
	tid        uint16
	quietUntil time.Time // RTU: no request before this
	quietTimer *time.Timer
	stats      LinkStats
	endErr     error // DTU: why the connection ended, if the server ended it
}

type call struct {
	unit     byte
	pdu      []byte
	done     func(resp []byte, err error)
	stop     func() bool // stops the context's AfterFunc
	tid      uint16
	tries    int
	timer    *time.Timer
	lastErr  error // CRC or framing error seen while waiting, reported instead of ErrTimeout
	finished bool  // done has been (or is about to be) called
}

// completion is a call's result, delivered once l.mu is released.
type completion struct {
	c    *call
	resp []byte
	err  error
}

func deliver(done []completion) {
	for _, d := range done {
		if d.c.stop != nil {
			d.c.stop()
		}
		d.c.done(d.resp, d.err)
	}
}

func newLink(cfg LinkConfig, id string) *Link {
	cfg.setDefaults()
	return &Link{cfg: cfg, id: id, inflight: make(map[uint16]*call), down: make(chan struct{}, 1)}
}

// ID returns the Link's address (Client) or DTU ID (DTUServer).
func (l *Link) ID() string { return l.id }

// Connected reports whether the Link has a connection.
func (l *Link) Connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil
}

// Stats returns the Link's counters.
func (l *Link) Stats() LinkStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats
}

// Close closes the Link and its connection; pending requests fail with ErrClosed. A Client's Link
// stops reconnecting.
func (l *Link) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	conn := l.conn
	done := l.failAllLocked(ErrClosed)
	l.mu.Unlock()
	if conn != nil {
		conn.AsyncClose()
	}
	select {
	case l.down <- struct{}{}: // wake the Client's connection goroutine
	default:
	}
	deliver(done)
	return nil
}

// failAllLocked ends every queued and outstanding request with err.
func (l *Link) failAllLocked(err error) []completion {
	var done []completion
	for _, c := range l.queue {
		done = l.finishLocked(done, c, nil, err)
	}
	l.queue = nil
	for id, c := range l.inflight {
		delete(l.inflight, id)
		c.timer.Stop()
		done = l.finishLocked(done, c, nil, err)
	}
	return done
}

func (l *Link) finishLocked(done []completion, c *call, resp []byte, err error) []completion {
	if c.finished {
		return done
	}
	c.finished = true
	return append(done, completion{c, resp, err})
}

// Go sends a request PDU (function code and data) to unit and calls done with the response PDU or
// an error; an exception response is reported as an *Exception. done runs on an event loop or a
// timer goroutine and must not block. ctx bounds the wait; when it ends first done gets ctx.Err().
func (l *Link) Go(ctx context.Context, unit byte, pdu []byte, done func(resp []byte, err error)) {
	if len(pdu) == 0 || len(pdu) > 253 {
		done(nil, fmt.Errorf("%w: PDU of %d bytes", ErrInvalidResponse, len(pdu)))
		return
	}
	if err := ctx.Err(); err != nil {
		done(nil, err)
		return
	}
	c := &call{unit: unit, pdu: append([]byte(nil), pdu...), done: done}
	// Set before the call is visible to other goroutines; if ctx ends at once, cancel finishes
	// the call and the queue skips it
	c.stop = context.AfterFunc(ctx, func() { l.cancel(c, ctx.Err()) })
	l.mu.Lock()
	if l.closed {
		finished := c.finished
		c.finished = true
		l.mu.Unlock()
		if !finished { // otherwise cancel has reported ctx.Err()
			c.stop()
			done(nil, ErrClosed)
		}
		return
	}
	l.queue = append(l.queue, c)
	conn, out, completed := l.pumpLocked(nil)
	l.mu.Unlock()
	if len(out) > 0 {
		conn.AsyncWrite(out, nil)
	}
	deliver(completed)
}

// Do sends a request PDU to unit and returns the response PDU; an exception response is returned
// as an *Exception error.
func (l *Link) Do(ctx context.Context, unit byte, pdu []byte) ([]byte, error) {
	type result struct {
		resp []byte
		err  error
	}
	ch := make(chan result, 1)
	l.Go(ctx, unit, pdu, func(resp []byte, err error) { ch <- result{resp, err} })
	r := <-ch
	if r.err != nil {
		return nil, r.err
	}
	if r.resp == nil { // broadcast
		return nil, nil
	}
	if err := checkResponse(pdu, r.resp); err != nil {
		return nil, err
	}
	return r.resp, nil
}

// cancel ends a request whose context is done: a queued one is removed; an outstanding one keeps its
// slot until its response or timeout (so RTU stays in step), but its caller is released now.
func (l *Link) cancel(c *call, err error) {
	l.mu.Lock()
	var done []completion
	for i, q := range l.queue {
		if q == c {
			l.queue = append(l.queue[:i], l.queue[i+1:]...)
			break
		}
	}
	done = l.finishLocked(done, c, nil, err)
	l.mu.Unlock()
	for _, d := range done {
		d.c.done(d.resp, d.err) // not deliver: the AfterFunc has already run
	}
}

// pumpLocked starts queued requests while the connection has room, and returns the frames to write
// and any broadcast requests completed by being sent.
func (l *Link) pumpLocked(done []completion) (*uringnet.Conn, []byte, []completion) {
	if l.conn == nil || l.closed {
		return nil, nil, done
	}
	rtu := l.cfg.Framing == RTUOverTCP
	if rtu {
		if wait := time.Until(l.quietUntil); wait > 0 {
			if l.quietTimer == nil {
				l.quietTimer = time.AfterFunc(wait, l.wake)
			}
			return nil, nil, done
		}
	}
	var out []byte
	for len(l.queue) > 0 && len(l.inflight) < l.cfg.MaxInflight {
		c := l.queue[0]
		l.queue[0] = nil
		l.queue = l.queue[1:]
		if c.finished { // cancelled while queued
			continue
		}
		c.tries++
		l.stats.Requests++
		if !rtu {
			for { // a transaction ID not in use
				l.tid++
				if l.inflight[l.tid] == nil {
					break
				}
			}
			c.tid = l.tid
			out = appendMBAP(out, c.tid, c.unit, c.pdu)
		} else {
			out = appendRTU(out, c.unit, c.pdu)
			if c.unit == 0 { // broadcast: no response; give the devices time to act
				done = l.finishLocked(done, c, nil, nil)
				l.quietUntil = time.Now().Add(l.cfg.Drain)
				break
			}
		}
		l.inflight[c.tid] = c
		c.timer = time.AfterFunc(l.cfg.Timeout, func() { l.timeout(c) })
	}
	if len(l.queue) == 0 {
		l.queue = nil
	}
	return l.conn, out, done
}

// wake resumes sending after an RTU quiet period.
func (l *Link) wake() {
	l.mu.Lock()
	l.quietTimer = nil
	conn, out, done := l.pumpLocked(nil)
	l.mu.Unlock()
	if len(out) > 0 {
		conn.AsyncWrite(out, nil)
	}
	deliver(done)
}

func (l *Link) timeout(c *call) {
	l.mu.Lock()
	if l.inflight[c.tid] != c {
		l.mu.Unlock()
		return
	}
	delete(l.inflight, c.tid)
	l.stats.Timeouts++
	if l.cfg.Framing == RTUOverTCP {
		l.quietUntil = time.Now().Add(l.cfg.Drain)
	}
	var done []completion
	if c.tries <= l.cfg.Retries && !c.finished {
		l.stats.Retries++
		l.queue = append([]*call{c}, l.queue...)
	} else {
		err := ErrTimeout
		if c.lastErr != nil {
			err = c.lastErr
		}
		done = l.finishLocked(done, c, nil, err)
	}
	conn, out, done := l.pumpLocked(done)
	l.mu.Unlock()
	if len(out) > 0 {
		conn.AsyncWrite(out, nil)
	}
	deliver(done)
}

// attach gives the Link its connection (on the connection's event loop).
func (l *Link) attach(c *uringnet.Conn) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		c.Close()
		return
	}
	l.conn = c
	conn, out, done := l.pumpLocked(nil)
	l.mu.Unlock()
	if len(out) > 0 {
		conn.Write(out)
	}
	deliver(done)
}

// detach handles the end of the connection: a Client's Link keeps its requests for the next
// connection (outstanding ones count as tried); a DTU Link closes.
func (l *Link) detach(c *uringnet.Conn) {
	l.mu.Lock()
	if l.conn != c {
		l.mu.Unlock()
		return
	}
	l.conn = nil
	var done []completion
	if l.client == nil {
		l.closed = true
		done = l.failAllLocked(ErrConnLost)
	} else {
		// Outstanding requests go back to the front of the queue, oldest first, if they may be retried
		var again []*call
		for id, x := range l.inflight {
			delete(l.inflight, id)
			x.timer.Stop()
			if x.tries <= l.cfg.Retries && !x.finished {
				again = append(again, x)
				l.stats.Retries++
			} else {
				done = l.finishLocked(done, x, nil, ErrConnLost)
			}
		}
		sortByTID(again)
		l.queue = append(again, l.queue...)
	}
	l.mu.Unlock()
	select {
	case l.down <- struct{}{}:
	default:
	}
	deliver(done)
}

func sortByTID(cs []*call) {
	for i := 1; i < len(cs); i++ { // few elements: insertion sort, wrapping IDs ignored
		for j := i; j > 0 && cs[j].tid < cs[j-1].tid; j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}

// onData parses responses from the connection (on its event loop) and reports whether the
// connection must be closed (unparseable Modbus TCP stream).
func (l *Link) onData(c *uringnet.Conn) uringnet.Action {
	rtu := l.cfg.Framing == RTUOverTCP
	var done []completion
	action := uringnet.None
	l.mu.Lock()
	for {
		buf, _ := c.Peek(-1)
		if len(buf) == 0 {
			break
		}
		if hb := l.heartbeat; len(hb) > 0 {
			if bytes.HasPrefix(buf, hb) {
				c.Discard(len(hb))
				continue
			}
			if len(buf) < len(hb) && bytes.HasPrefix(hb, buf) {
				break // possibly the start of a heartbeat
			}
		}
		var n int
		if rtu {
			n = rtuFrameLen(buf, false)
		} else {
			n = tcpFrameLen(buf)
		}
		if n == 0 {
			break
		}
		if n < 0 {
			l.stats.Dropped++
			if !rtu {
				action = uringnet.Close // the stream cannot be resynchronized
				break
			}
			// RTU: drop what has arrived and start over with the next bytes
			c.Discard(-1)
			l.noteLocked(ErrInvalidResponse)
			break
		}
		done = l.frameLocked(buf[:n], rtu, done)
		c.Discard(n)
	}
	conn, out, done := l.pumpLocked(done)
	l.mu.Unlock()
	if len(out) > 0 && action == uringnet.None {
		conn.Write(out)
	}
	deliver(done)
	return action
}

// noteLocked records an error for the outstanding RTU request, reported if it then times out.
func (l *Link) noteLocked(err error) {
	if x := l.inflight[0]; x != nil {
		x.lastErr = err
	}
}

// frameLocked matches one response frame to its request.
func (l *Link) frameLocked(frame []byte, rtu bool, done []completion) []completion {
	var c *call
	var pdu []byte
	if rtu {
		if !checkCRC(frame) {
			l.stats.Dropped++
			l.noteLocked(ErrCRC)
			return done
		}
		c, pdu = l.inflight[0], frame[1:len(frame)-2]
		if c != nil && (frame[0] != c.unit || !plausible(c.pdu, pdu)) {
			c = nil // a late response to an earlier request
		}
	} else {
		c, pdu = l.inflight[binary.BigEndian.Uint16(frame)], frame[mbapLen:]
	}
	if c == nil || pdu[0]&0x7f != c.pdu[0] {
		l.stats.Dropped++ // late, unknown, or for another function
		return done
	}
	delete(l.inflight, c.tid)
	c.timer.Stop()
	l.stats.Responses++
	return l.finishLocked(done, c, append([]byte(nil), pdu...), nil)
}

// Typed requests.

func checkQty(n, max int) error {
	if n < 1 || n > max {
		return fmt.Errorf("%w: %d (1 to %d)", ErrQuantity, n, max)
	}
	return nil
}

func (l *Link) readBits(ctx context.Context, fc, unit byte, addr, qty uint16) ([]bool, error) {
	if err := checkQty(int(qty), MaxReadBits); err != nil {
		return nil, err
	}
	req := pduAddrQty(fc, addr, qty)
	resp, err := l.Do(ctx, unit, req)
	if err != nil || resp == nil {
		return nil, err
	}
	return decodeBits(req, resp)
}

func (l *Link) readRegisters(ctx context.Context, req []byte, unit byte) ([]uint16, error) {
	resp, err := l.Do(ctx, unit, req)
	if err != nil || resp == nil {
		return nil, err
	}
	return decodeRegisters(req, resp)
}

func (l *Link) write(ctx context.Context, unit byte, req []byte) error {
	resp, err := l.Do(ctx, unit, req)
	if err != nil || resp == nil { // nil: broadcast
		return err
	}
	return checkEcho(req, resp)
}

// ReadCoils reads qty coils (function 0x01).
func (l *Link) ReadCoils(ctx context.Context, unit byte, addr, qty uint16) ([]bool, error) {
	return l.readBits(ctx, FuncReadCoils, unit, addr, qty)
}

// ReadDiscreteInputs reads qty discrete inputs (function 0x02).
func (l *Link) ReadDiscreteInputs(ctx context.Context, unit byte, addr, qty uint16) ([]bool, error) {
	return l.readBits(ctx, FuncReadDiscreteInputs, unit, addr, qty)
}

// ReadHoldingRegisters reads qty holding registers (function 0x03).
func (l *Link) ReadHoldingRegisters(ctx context.Context, unit byte, addr, qty uint16) ([]uint16, error) {
	if err := checkQty(int(qty), MaxReadRegisters); err != nil {
		return nil, err
	}
	return l.readRegisters(ctx, pduAddrQty(FuncReadHoldingRegisters, addr, qty), unit)
}

// ReadInputRegisters reads qty input registers (function 0x04).
func (l *Link) ReadInputRegisters(ctx context.Context, unit byte, addr, qty uint16) ([]uint16, error) {
	if err := checkQty(int(qty), MaxReadRegisters); err != nil {
		return nil, err
	}
	return l.readRegisters(ctx, pduAddrQty(FuncReadInputRegisters, addr, qty), unit)
}

// WriteSingleCoil writes one coil (function 0x05). Unit 0 broadcasts over RTU (no response).
func (l *Link) WriteSingleCoil(ctx context.Context, unit byte, addr uint16, v bool) error {
	val := uint16(0)
	if v {
		val = 0xFF00
	}
	return l.write(ctx, unit, pduAddrQty(FuncWriteSingleCoil, addr, val))
}

// WriteSingleRegister writes one holding register (function 0x06).
func (l *Link) WriteSingleRegister(ctx context.Context, unit byte, addr, v uint16) error {
	return l.write(ctx, unit, pduAddrQty(FuncWriteSingleRegister, addr, v))
}

// WriteMultipleCoils writes coils (function 0x0F).
func (l *Link) WriteMultipleCoils(ctx context.Context, unit byte, addr uint16, values []bool) error {
	if err := checkQty(len(values), MaxWriteBits); err != nil {
		return err
	}
	return l.write(ctx, unit, pduWriteCoils(addr, values))
}

// WriteMultipleRegisters writes holding registers (function 0x10).
func (l *Link) WriteMultipleRegisters(ctx context.Context, unit byte, addr uint16, values []uint16) error {
	if err := checkQty(len(values), MaxWriteRegisters); err != nil {
		return err
	}
	return l.write(ctx, unit, pduWriteRegisters(addr, values))
}

// ReadWriteMultipleRegisters writes values at writeAddr, then reads readQty registers at readAddr,
// in one transaction (function 0x17).
func (l *Link) ReadWriteMultipleRegisters(ctx context.Context, unit byte, readAddr, readQty, writeAddr uint16, values []uint16) ([]uint16, error) {
	if err := checkQty(int(readQty), MaxReadRegisters); err != nil {
		return nil, err
	}
	if err := checkQty(len(values), maxRWWrite); err != nil {
		return nil, err
	}
	return l.readRegisters(ctx, pduReadWrite(readAddr, readQty, writeAddr, values), unit)
}
