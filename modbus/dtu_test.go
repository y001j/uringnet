//go:build linux
// +build linux

package modbus

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// fakeDTU connects to a DTU server, registers, and answers RTU requests from a Memory, sending a
// heartbeat before every other response.
func fakeDTU(t *testing.T, addr, imei string, m *Memory, heartbeat []byte) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.Write([]byte(imei))
	go func() {
		d := &device{t: t, rtu: true}
		var pending []byte
		for i := 0; ; i++ {
			_, unit, pdu, err := d.readRequest(c, &pending)
			if err != nil {
				return
			}
			resp := appendRTU(nil, unit, serve(m, unit, pdu))
			if i%2 == 0 {
				resp = append(append([]byte(nil), heartbeat...), resp...) // heartbeat glued to the response
			}
			// Split the response in two writes, as a DTU forwarding serial bytes may
			c.Write(resp[:3])
			time.Sleep(5 * time.Millisecond)
			c.Write(resp[3:])
			if i%3 == 0 {
				c.Write(heartbeat) // a heartbeat on its own
			}
		}
	}()
	return c
}

func TestDTUServer(t *testing.T) {
	type event struct {
		id     string
		online bool
		err    error
		link   *Link
	}
	events := make(chan event, 16)
	hb := []byte("HEARTBEAT")
	s := NewDTUServer(DTUConfig{
		LinkConfig:      LinkConfig{Framing: RTUOverTCP, Timeout: 500 * time.Millisecond},
		Register:        RegisterFixed(15),
		RegisterTimeout: 300 * time.Millisecond,
		Heartbeat:       hb,
		Authorize:       func(id, _ string) bool { return id != "000000000000000" },
		OnOnline:        func(l *Link) { events <- event{l.ID(), true, nil, l} },
		OnOffline:       func(l *Link, err error) { events <- event{l.ID(), false, err, l} },
	})
	addr := listen(t, s.Handler())
	next := func() event {
		t.Helper()
		select {
		case e := <-events:
			return e
		case <-time.After(3 * time.Second):
			t.Fatal("no DTU event")
		}
		return event{}
	}

	m1, m2 := NewMemory(8, 0, 10, 0), NewMemory(8, 0, 10, 0)
	m1.SetHoldingRegisters(0, []uint16{1, 11})
	m2.SetHoldingRegisters(0, []uint16{2, 22})
	fakeDTU(t, addr, "860000000000001", m1, hb)
	fakeDTU(t, addr, "860000000000002", m2, hb)
	for i := 0; i < 2; i++ {
		if e := next(); !e.online {
			t.Fatalf("%+v", e)
		}
	}
	if ids := s.IDs(); len(ids) != 2 || ids[0] != "860000000000001" {
		t.Fatalf("IDs %v", ids)
	}
	ctx := ctxT(t)
	for round := 0; round < 5; round++ {
		for id, want := range map[string]uint16{"860000000000001": 11, "860000000000002": 22} {
			regs, err := s.Link(id).ReadHoldingRegisters(ctx, 1, 0, 2)
			if err != nil || regs[1] != want {
				t.Fatalf("%s round %d: %v %v", id, round, regs, err)
			}
		}
	}
	l1 := s.Link("860000000000001")
	if err := l1.WriteSingleCoil(ctx, 1, 2, true); err != nil {
		t.Fatal(err)
	}
	if coils, _ := m1.Coils(0, 3); !coils[2] {
		t.Fatal("write not performed")
	}
	if st := l1.Stats(); st.Dropped != 0 || st.Timeouts != 0 {
		t.Fatalf("heartbeats disturbed the link: %+v", st)
	}

	// The same DTU connects again (its old connection silently dead): the new connection replaces it
	m1b := NewMemory(0, 0, 10, 0)
	m1b.SetHoldingRegisters(0, []uint16{100})
	fakeDTU(t, addr, "860000000000001", m1b, hb)
	e1, e2 := next(), next()
	if e1.online {
		e1, e2 = e2, e1
	}
	if e1.online || !errors.Is(e1.err, ErrReplaced) || e1.link != l1 || !e2.online || e2.link == l1 {
		t.Fatalf("replacement events %+v %+v", e1, e2)
	}
	if regs, err := s.Link("860000000000001").ReadHoldingRegisters(ctx, 1, 0, 1); err != nil || regs[0] != 100 {
		t.Fatalf("new connection: %v %v", regs, err)
	}
	if _, err := l1.ReadHoldingRegisters(ctx, 1, 0, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("old link: %v", err)
	}

	// A DTU that disconnects goes offline; its Link is gone
	for _, id := range []string{"860000000000002"} {
		s.Link(id).Close()
		if e := next(); e.online || e.id != id {
			t.Fatalf("%+v", e)
		}
		if s.Link(id) != nil {
			t.Fatal("closed DTU still listed")
		}
	}

	// No registration in time, and an unauthorized ID: closed by the server
	for _, first := range []string{"", "000000000000000"} {
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(first))
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("registration %q: connection not closed: %v", first, err)
		}
		c.Close()
	}
	if len(s.IDs()) != 1 {
		t.Fatalf("IDs %v", s.IDs())
	}
}

// Without a registration packet the DTU's ID is its address.
func TestDTUWithoutRegistration(t *testing.T) {
	online := make(chan *Link, 1)
	s := NewDTUServer(DTUConfig{LinkConfig: LinkConfig{Framing: TCP}, OnOnline: func(l *Link) { online <- l }})
	addr := listen(t, s.Handler())
	m := NewMemory(0, 0, 4, 0)
	m.SetHoldingRegisters(0, []uint16{5})
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go func() { // a Modbus TCP slave on the connection it opened
		d := &device{t: t}
		var pending []byte
		for {
			tid, unit, pdu, err := d.readRequest(c, &pending)
			if err != nil {
				return
			}
			c.Write(appendMBAP(nil, tid, unit, serve(m, unit, pdu)))
		}
	}()
	l := <-online
	if l.ID() != c.LocalAddr().String() {
		t.Fatalf("ID %q, want %q", l.ID(), c.LocalAddr())
	}
	if regs, err := l.ReadHoldingRegisters(ctxT(t), 1, 0, 1); err != nil || regs[0] != 5 {
		t.Fatalf("%v %v", regs, err)
	}
}
