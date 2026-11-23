//go:build linux
// +build linux

package modbus

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/y001j/uringnet"
)

func backendOpts(t *testing.T) []uringnet.Option {
	opts := []uringnet.Option{uringnet.WithRings(2)}
	if os.Getenv("URINGNET_BACKEND") == "epoll" { // as in the uringnet package tests
		opts = append(opts, uringnet.WithBackend(uringnet.BackendEpoll))
	}
	return opts
}

func listen(t *testing.T, h uringnet.Handler) string {
	t.Helper()
	srv, err := uringnet.Listen("tcp4", "127.0.0.1:0", h, backendOpts(t)...)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("URINGNET_BACKEND") == "epoll" && srv.Backend() != uringnet.BackendEpoll {
		t.Fatal("not on the epoll backend")
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	return srv.Addr().String()
}

func newClient(t *testing.T, cfg ClientConfig) *Client {
	t.Helper()
	c, err := NewClient(cfg, backendOpts(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// device is a scripted device on a plain net.Listener.
type device struct {
	t   *testing.T
	ln  net.Listener
	rtu bool
}

func newDevice(t *testing.T, rtu bool, serveConn func(d *device, c net.Conn)) *device {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &device{t: t, ln: ln, rtu: rtu}
	go d.accept(ln, serveConn)
	t.Cleanup(func() { ln.Close() })
	return d
}

// accept serves ln; it takes the listener as a parameter so that a test can replace a device's
// listener without racing with the goroutine serving the old one.
func (d *device) accept(ln net.Listener, serveConn func(d *device, c net.Conn)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() { defer c.Close(); serveConn(d, c) }()
	}
}

func (d *device) addr() string { return d.ln.Addr().String() }

// readRequest reads one request frame: (transaction ID, unit, PDU).
func (d *device) readRequest(c net.Conn, pending *[]byte) (uint16, byte, []byte, error) {
	buf := make([]byte, 512)
	for {
		var n int
		if d.rtu {
			n = rtuFrameLen(*pending, true)
		} else {
			n = tcpFrameLen(*pending)
		}
		if n > 0 {
			f := append([]byte(nil), (*pending)[:n]...)
			*pending = (*pending)[n:]
			if d.rtu {
				if !checkCRC(f) {
					d.t.Errorf("request with a bad CRC % x", f)
				}
				return 0, f[0], f[1 : n-2], nil
			}
			return uint16(f[0])<<8 | uint16(f[1]), f[6], f[mbapLen:], nil
		}
		if n < 0 {
			d.t.Errorf("unparseable request % x", *pending)
			return 0, 0, nil, errors.New("bad request")
		}
		k, err := c.Read(buf)
		if err != nil {
			return 0, 0, nil, err
		}
		*pending = append(*pending, buf[:k]...)
	}
}

func (d *device) response(tid uint16, unit byte, pdu []byte) []byte {
	if d.rtu {
		return appendRTU(nil, unit, pdu)
	}
	return appendMBAP(nil, tid, unit, pdu)
}

func TestClientServer(t *testing.T) {
	for _, framing := range []Framing{TCP, RTUOverTCP} {
		t.Run(framing.String(), func(t *testing.T) {
			m := NewMemory(100, 100, 200, 200)
			m.SetDiscreteInputs(10, []bool{true, true, false, true})
			m.SetInputRegisters(5, []uint16{0xAAAA, 0xBBBB})
			srv := NewServer(m, framing)
			addr := listen(t, srv.Handler())
			c := newClient(t, ClientConfig{LinkConfig: LinkConfig{Framing: framing}})
			l := c.Dial(addr)
			ctx := ctxT(t)

			if err := l.WriteSingleCoil(ctx, 1, 3, true); err != nil {
				t.Fatal(err)
			}
			if err := l.WriteMultipleCoils(ctx, 1, 20, []bool{true, false, true, true, false, false, false, false, true}); err != nil {
				t.Fatal(err)
			}
			if bits, err := l.ReadCoils(ctx, 1, 0, 30); err != nil || !bits[3] || !bits[20] || bits[21] || !bits[28] || bits[29] {
				t.Fatalf("coils %v %v", bits, err)
			}
			if bits, err := l.ReadDiscreteInputs(ctx, 1, 10, 4); err != nil || !bits[0] || bits[2] || !bits[3] {
				t.Fatalf("inputs %v %v", bits, err)
			}
			if err := l.WriteSingleRegister(ctx, 1, 0, 0x1234); err != nil {
				t.Fatal(err)
			}
			if err := l.WriteMultipleRegisters(ctx, 1, 1, []uint16{1, 2, 3}); err != nil {
				t.Fatal(err)
			}
			if regs, err := l.ReadHoldingRegisters(ctx, 1, 0, 4); err != nil || regs[0] != 0x1234 || regs[3] != 3 {
				t.Fatalf("holding %v %v", regs, err)
			}
			if regs, err := l.ReadInputRegisters(ctx, 1, 5, 2); err != nil || regs[1] != 0xBBBB {
				t.Fatalf("input %v %v", regs, err)
			}
			if regs, err := l.ReadWriteMultipleRegisters(ctx, 1, 0, 2, 100, []uint16{7, 8}); err != nil || regs[0] != 0x1234 {
				t.Fatalf("read/write %v %v", regs, err)
			}
			if got, _ := m.HoldingRegisters(100, 2); got[1] != 8 {
				t.Fatalf("written %v", got)
			}
			// 125 registers, the largest read
			if regs, err := l.ReadHoldingRegisters(ctx, 1, 0, 125); err != nil || len(regs) != 125 {
				t.Fatalf("%d registers, %v", len(regs), err)
			}

			// Exceptions and local checks
			_, err := l.ReadHoldingRegisters(ctx, 1, 199, 2)
			var e *Exception
			if !errors.As(err, &e) || !errors.Is(err, IllegalDataAddress) || e.Function != FuncReadHoldingRegisters {
				t.Fatalf("out of range: %v", err)
			}
			if _, err := l.Do(ctx, 1, []byte{0x08, 0, 0, 0x12, 0x34}); !errors.Is(err, IllegalFunction) && framing == TCP {
				t.Fatalf("diagnostics: %v", err)
			}
			if _, err := l.ReadHoldingRegisters(ctx, 1, 0, 126); !errors.Is(err, ErrQuantity) {
				t.Fatalf("126 registers: %v", err)
			}

			if framing == RTUOverTCP {
				// Unsupported functions cannot be framed in RTU: the server drops them and the request
				// times out (shortened here)
				short, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
				defer cancel()
				if _, err := l.Do(short, 1, []byte{0x08, 0, 0, 0x12, 0x34}); !errors.Is(err, ErrTimeout) {
					t.Fatalf("RTU diagnostics: %v", err)
				}
				// Unit 0 broadcasts a write: no response, but it is performed
				if err := l.WriteSingleRegister(ctx, 0, 50, 0x5555); err != nil {
					t.Fatal(err)
				}
				if regs, err := l.ReadHoldingRegisters(ctx, 1, 50, 1); err != nil || regs[0] != 0x5555 {
					t.Fatalf("after broadcast %v %v", regs, err)
				}
			}
			if st := l.Stats(); st.Dropped != 0 || st.Requests != st.Responses+st.Timeouts+boolU(framing == RTUOverTCP) {
				t.Fatalf("stats %+v", st)
			}
		})
	}
}

func boolU(b bool) uint64 {
	if b {
		return 1 // the broadcast has no response
	}
	return 0
}

// Several Modbus TCP requests outstanding; responses matched by transaction ID, in any order.
func TestPipelining(t *testing.T) {
	const n = 4
	d := newDevice(t, false, func(d *device, c net.Conn) {
		var pending []byte
		for {
			type req struct {
				tid  uint16
				unit byte
				pdu  []byte
			}
			var reqs []req
			for len(reqs) < n { // read all before answering: only possible if they were pipelined
				tid, unit, pdu, err := d.readRequest(c, &pending)
				if err != nil {
					return
				}
				reqs = append(reqs, req{tid, unit, pdu})
			}
			for i := n - 1; i >= 0; i-- { // answer in reverse order; the value is the address
				r := reqs[i]
				c.Write(d.response(r.tid, r.unit, []byte{3, 2, r.pdu[1], r.pdu[2]}))
			}
		}
	})
	c := newClient(t, ClientConfig{LinkConfig: LinkConfig{MaxInflight: n}})
	l := c.Dial(d.addr())
	ctx := ctxT(t)
	var wg sync.WaitGroup
	for round := 0; round < 3; round++ {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(addr uint16) {
				defer wg.Done()
				regs, err := l.ReadHoldingRegisters(ctx, 1, addr, 1)
				if err != nil || regs[0] != addr {
					t.Errorf("address %d: %v %v", addr, regs, err)
				}
			}(uint16(round*100 + i))
		}
		wg.Wait()
	}
}

func TestTimeoutAndRetry(t *testing.T) {
	var seen atomic.Int32
	d := newDevice(t, true, func(d *device, c net.Conn) {
		var pending []byte
		for {
			_, unit, pdu, err := d.readRequest(c, &pending)
			if err != nil {
				return
			}
			if seen.Add(1)%2 == 1 {
				continue // ignore every other request
			}
			c.Write(d.response(0, unit, []byte{pdu[0], 2, 0, 42}))
		}
	})
	c := newClient(t, ClientConfig{LinkConfig: LinkConfig{Framing: RTUOverTCP, Timeout: 100 * time.Millisecond, Retries: 1}})
	l := c.Dial(d.addr())
	regs, err := l.ReadHoldingRegisters(ctxT(t), 1, 0, 1)
	if err != nil || regs[0] != 42 {
		t.Fatalf("%v %v", regs, err)
	}
	if st := l.Stats(); st.Timeouts != 1 || st.Retries != 1 || st.Requests != 2 || st.Responses != 1 {
		t.Fatalf("stats %+v", st)
	}

	c0 := newClient(t, ClientConfig{LinkConfig: LinkConfig{Framing: RTUOverTCP, Timeout: 100 * time.Millisecond}})
	l0 := c0.Dial(d.addr())
	start := time.Now()
	if _, err := l0.ReadHoldingRegisters(ctxT(t), 1, 0, 1); !errors.Is(err, ErrTimeout) {
		t.Fatalf("without retries: %v", err)
	}
	if el := time.Since(start); el < 100*time.Millisecond || el > time.Second {
		t.Fatalf("timed out after %v", el)
	}
}

// RTU has no transaction IDs: a response arriving after its timeout must not be taken for the
// answer to the next request.
func TestRTULateResponse(t *testing.T) {
	d := newDevice(t, true, func(d *device, c net.Conn) {
		var pending []byte
		first := true
		for {
			_, unit, pdu, err := d.readRequest(c, &pending)
			if err != nil {
				return
			}
			resp := d.response(0, unit, []byte{pdu[0], 2, 0, pdu[2]}) // value: the address
			if first {
				first = false
				go func() { time.Sleep(300 * time.Millisecond); c.Write(resp) }()
				continue
			}
			c.Write(resp)
		}
	})
	// The late response arrives 200ms after the timeout, during the drain: nothing is outstanding
	c := newClient(t, ClientConfig{LinkConfig: LinkConfig{Framing: RTUOverTCP, Timeout: 100 * time.Millisecond, Drain: 400 * time.Millisecond}})
	l := c.Dial(d.addr())
	ctx := ctxT(t)
	if _, err := l.ReadHoldingRegisters(ctx, 1, 1, 1); !errors.Is(err, ErrTimeout) {
		t.Fatal(err)
	}
	for i := 2; i < 6; i++ {
		regs, err := l.ReadHoldingRegisters(ctx, 1, uint16(i), 1)
		if err != nil || regs[0] != uint16(i) {
			t.Fatalf("request %d got %v %v", i, regs, err)
		}
	}
	if st := l.Stats(); st.Dropped != 1 {
		t.Fatalf("late response not dropped: %+v", st)
	}

	// With a short drain the late response arrives while another request is outstanding: it is
	// recognized because it does not fit that request (2 registers asked, 1 returned)
	c2 := newClient(t, ClientConfig{LinkConfig: LinkConfig{Framing: RTUOverTCP, Timeout: 250 * time.Millisecond, Drain: 10 * time.Millisecond}})
	l2 := c2.Dial(newDevice(t, true, lateFirst).addr())
	if _, err := l2.ReadHoldingRegisters(ctx, 1, 1, 1); !errors.Is(err, ErrTimeout) {
		t.Fatal(err)
	}
	if regs, err := l2.ReadHoldingRegisters(ctx, 1, 2, 2); err != nil || regs[0] != 2 || regs[1] != 2 {
		t.Fatalf("got %v %v", regs, err)
	}
	if st := l2.Stats(); st.Dropped != 1 {
		t.Fatalf("late response not dropped: %+v", st)
	}
}

// lateFirst answers the first request after 300ms (50ms after a 250ms timeout) and the others after
// 100ms, so the late answer arrives while the second request is outstanding. Registers hold their
// address.
func lateFirst(d *device, c net.Conn) {
	var pending []byte
	for i := 0; ; i++ {
		_, unit, pdu, err := d.readRequest(c, &pending)
		if err != nil {
			return
		}
		qty := int(pdu[4])
		resp := []byte{pdu[0], byte(2 * qty)}
		for k := 0; k < qty; k++ {
			resp = append(resp, 0, pdu[2])
		}
		frame := d.response(0, unit, resp)
		if i == 0 {
			go func() { time.Sleep(300 * time.Millisecond); c.Write(frame) }()
			continue
		}
		time.Sleep(100 * time.Millisecond)
		c.Write(frame)
	}
}

func TestRTUBadCRCAndGarbage(t *testing.T) {
	var mode atomic.Int32
	d := newDevice(t, true, func(d *device, c net.Conn) {
		var pending []byte
		for {
			_, unit, pdu, err := d.readRequest(c, &pending)
			if err != nil {
				return
			}
			resp := d.response(0, unit, []byte{pdu[0], 2, 0, 1})
			switch mode.Load() {
			case 0:
				resp[len(resp)-1] ^= 0xFF
			case 1:
				resp = []byte{0x01, 0x41, 0x99} // unknown function: cannot be framed
			}
			c.Write(resp)
		}
	})
	c := newClient(t, ClientConfig{LinkConfig: LinkConfig{Framing: RTUOverTCP, Timeout: 100 * time.Millisecond}})
	l := c.Dial(d.addr())
	ctx := ctxT(t)
	if _, err := l.ReadHoldingRegisters(ctx, 1, 0, 1); !errors.Is(err, ErrCRC) {
		t.Fatalf("bad CRC: %v", err)
	}
	mode.Store(1)
	if _, err := l.ReadHoldingRegisters(ctx, 1, 0, 1); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("garbage: %v", err)
	}
	mode.Store(2) // the link resynchronizes
	if regs, err := l.ReadHoldingRegisters(ctx, 1, 0, 1); err != nil || regs[0] != 1 {
		t.Fatalf("after garbage: %v %v", regs, err)
	}
}

func TestReconnect(t *testing.T) {
	var conns atomic.Int32
	handler := func(d *device, c net.Conn) {
		conns.Add(1)
		var pending []byte
		tid, unit, pdu, err := d.readRequest(c, &pending)
		if err != nil {
			return
		}
		c.Write(d.response(tid, unit, []byte{pdu[0], 2, 0, 7}))
		// then drop the connection
	}
	d := newDevice(t, false, handler)
	addr := d.addr()
	var up, down atomic.Int32
	// Retries: a request sent just before the device drops the connection is sent again
	c := newClient(t, ClientConfig{LinkConfig: LinkConfig{Retries: 1}, ReconnectMin: 50 * time.Millisecond, ReconnectMax: 200 * time.Millisecond,
		OnConnect: func(*Link) { up.Add(1) }, OnDisconnect: func(*Link, error) { down.Add(1) }})
	l := c.Dial(addr)
	ctx := ctxT(t)
	for i := 0; i < 3; i++ { // each request is served by a new connection
		if regs, err := l.ReadHoldingRegisters(ctx, 1, 0, 1); err != nil || regs[0] != 7 {
			t.Fatalf("request %d: %v %v", i, regs, err)
		}
	}
	if conns.Load() < 3 || up.Load() < 3 {
		t.Fatalf("%d connections, %d OnConnect", conns.Load(), up.Load())
	}

	// The device goes away; a request waits for it to come back
	d.ln.Close()
	time.Sleep(100 * time.Millisecond)
	res := make(chan error, 1)
	go func() {
		_, err := l.ReadHoldingRegisters(ctx, 1, 0, 1)
		res <- err
	}()
	time.Sleep(400 * time.Millisecond)
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Skipf("cannot listen on %s again: %v", addr, err)
	}
	t.Cleanup(func() { ln.Close() })
	go d.accept(ln, handler)
	if err := <-res; err != nil {
		t.Fatalf("request across the outage: %v", err)
	}
	if down.Load() == 0 {
		t.Fatal("no OnDisconnect")
	}
}

func TestContextAndClose(t *testing.T) {
	d := newDevice(t, false, func(d *device, c net.Conn) { // never answers
		var pending []byte
		for {
			if _, _, _, err := d.readRequest(c, &pending); err != nil {
				return
			}
		}
	})
	c := newClient(t, ClientConfig{LinkConfig: LinkConfig{Timeout: 500 * time.Millisecond}})
	l := c.Dial(d.addr())
	first := make(chan error, 1)
	go func() {
		_, err := l.ReadHoldingRegisters(ctxT(t), 1, 0, 1)
		first <- err
	}()
	time.Sleep(50 * time.Millisecond)
	// Queued behind the first: its context ends first
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := l.ReadHoldingRegisters(short, 1, 0, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued request: %v", err)
	}
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Fatalf("returned after %v", el)
	}
	if err := <-first; !errors.Is(err, ErrTimeout) {
		t.Fatalf("first request: %v", err)
	}

	// Close fails pending requests and later ones
	pending := make(chan error, 1)
	go func() {
		_, err := l.ReadHoldingRegisters(ctxT(t), 1, 0, 1)
		pending <- err
	}()
	time.Sleep(50 * time.Millisecond)
	l.Close()
	if err := <-pending; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending at close: %v", err)
	}
	if _, err := l.ReadHoldingRegisters(ctxT(t), 1, 0, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close: %v", err)
	}
}

// A server with RTU framing ignores frames with a bad CRC and answers the next valid one.
func TestServerRTUBadFrames(t *testing.T) {
	m := NewMemory(0, 0, 10, 0)
	m.SetHoldingRegisters(0, []uint16{9})
	srv := NewServer(m, RTUOverTCP)
	addr := listen(t, srv.Handler())
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	bad := appendRTU(nil, 1, pduAddrQty(FuncReadHoldingRegisters, 0, 1))
	bad[len(bad)-1] ^= 1
	c.Write(bad)
	time.Sleep(50 * time.Millisecond)
	c.Write(appendRTU(nil, 1, pduAddrQty(FuncReadHoldingRegisters, 0, 1)))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || rtuFrameLen(buf[:n], false) != n || !checkCRC(buf[:n]) || buf[4] != 9 {
		t.Fatalf("% x %v", buf[:n], err)
	}
	if st := srv.Stats(); st.Dropped != 1 || st.Requests != 1 {
		t.Fatalf("%+v", st)
	}
}
