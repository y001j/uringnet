//go:build linux
// +build linux

package modbus

import (
	"errors"
	"math"
	"net"
	"sync"
	"testing"
	"time"
)

// collector gathers the poller's deliveries.
type collector struct {
	mu  sync.Mutex
	got map[string][]Value // by device, latest poll per point appended
	ch  chan string
}

func newCollector() *collector {
	return &collector{got: map[string][]Value{}, ch: make(chan string, 1024)}
}

func (c *collector) on(device string, values []Value) {
	c.mu.Lock()
	c.got[device] = append(c.got[device], values...)
	c.mu.Unlock()
	c.ch <- device
}

func (c *collector) wait(t *testing.T, device string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case d := <-c.ch:
			if d == device {
				return
			}
		case <-timeout:
			t.Fatalf("no values from %s", device)
		}
	}
}

func valueOf(vs []Value, name string) (Value, bool) {
	for i := len(vs) - 1; i >= 0; i-- {
		if vs[i].Point.Name == name {
			return vs[i], true
		}
	}
	return Value{}, false
}

func meterPoints() []Point {
	return []Point{
		{Name: "voltage", Table: InputRegister, Address: 0, Type: Float32, Order: CDAB},
		{Name: "current", Table: InputRegister, Address: 2, Scale: 0.01},
		{Name: "energy", Table: InputRegister, Address: 3, Type: Uint32},
		{Name: "setpoint", Address: 10, Type: Int16},
		{Name: "mode", Address: 11},
		{Name: "label", Address: 20, Type: String, Length: 3},
		{Name: "alarm", Address: 11, Type: Bool, Bit: 1},
		{Name: "relay", Table: Coil, Address: 0, Type: Bool},
		{Name: "door", Table: DiscreteInput, Address: 5, Type: Bool},
		{Name: "fast", Table: InputRegister, Address: 50, Interval: 50 * time.Millisecond},
	}
}

func meterMemory() *Memory {
	m := NewMemory(8, 8, 32, 64)
	m.SetInputRegisters(0, []uint16{0x0000, 0x4366, 2345, 0x0001, 0x86A0}) // 230.0 (CDAB), 23.45 A, 100000
	m.SetHoldingRegisters(10, []uint16{0xFFF6, 0x0002})                    // -10, mode 2 (alarm bit 1 set)
	m.SetHoldingRegisters(20, []uint16{0x4142, 0x4300, 0})                 // "ABC"
	m.SetCoils(0, []bool{true})
	m.SetDiscreteInputs(5, []bool{true})
	return m
}

func TestPoller(t *testing.T) {
	m := meterMemory()
	addr := listen(t, NewServer(m, TCP).Handler())
	c := newClient(t, ClientConfig{})
	link := c.Dial(addr)
	col := newCollector()
	p := NewPoller(PollerConfig{Interval: 200 * time.Millisecond, OnValues: col.on})
	defer p.Close()
	if err := p.Add(Device{Name: "meter", Link: LinkOf(link), Points: meterPoints()}); err != nil {
		t.Fatal(err)
	}
	// Wait for a poll of the slow group (which has all points but "fast")
	deadline := time.Now().Add(5 * time.Second)
	for {
		col.wait(t, "meter")
		col.mu.Lock()
		_, ok := valueOf(col.got["meter"], "voltage")
		col.mu.Unlock()
		if ok || time.Now().After(deadline) {
			break
		}
	}
	col.mu.Lock()
	vs := col.got["meter"]
	col.mu.Unlock()
	for name, want := range map[string]any{
		"voltage": 230.0, "current": 23.45, "energy": 100000.0, "setpoint": -10.0, "mode": 2.0,
		"label": "ABC", "alarm": true, "relay": true, "door": true,
	} {
		v, ok := valueOf(vs, name)
		if !ok || v.Err != nil {
			t.Fatalf("%s: %+v", name, v)
		}
		if f, isF := want.(float64); isF {
			if math.Abs(v.Value.(float64)-f) > 1e-9 {
				t.Errorf("%s = %v, want %v", name, v.Value, want)
			}
		} else if v.Value != want {
			t.Errorf("%s = %v, want %v", name, v.Value, want)
		}
	}
	// Requests per slow poll: input 0-4, holding 10-11, holding 20-22, coil, discrete = 5
	time.Sleep(500 * time.Millisecond)
	st := p.Stats()
	if st.Errors != 0 || st.Requests == 0 {
		t.Fatalf("stats %+v", st)
	}
	// The fast group (50ms) polls far more often than the slow one (200ms)
	col.mu.Lock()
	fast, slow := 0, 0
	for _, v := range col.got["meter"] {
		switch v.Point.Name {
		case "fast":
			fast++
		case "voltage":
			slow++
		}
	}
	col.mu.Unlock()
	if fast < 2*slow || slow < 2 {
		t.Fatalf("fast polled %d times, slow %d", fast, slow)
	}
	if last := p.Last("meter"); len(last) != len(meterPoints()) {
		t.Fatalf("Last: %d values", len(last))
	}

	// Writes through the poller, encoded by the point's type
	ctx := ctxT(t)
	if err := p.Write(ctx, "meter", "setpoint", -25); err != nil {
		t.Fatal(err)
	}
	if err := p.Write(ctx, "meter", "relay", "off"); err != nil {
		t.Fatal(err)
	}
	if err := p.Write(ctx, "meter", "label", "XY"); err != nil {
		t.Fatal(err)
	}
	if regs, _ := m.HoldingRegisters(10, 1); regs[0] != 0xFFE7 {
		t.Fatalf("setpoint register % x", regs)
	}
	if coils, _ := m.Coils(0, 1); coils[0] {
		t.Fatal("relay still on")
	}
	if regs, _ := m.HoldingRegisters(20, 3); regs[0] != 0x5859 || regs[1] != 0 {
		t.Fatalf("label % x", regs)
	}
	for _, bad := range []struct{ point string }{{"voltage"}, {"alarm"}, {"nope"}} {
		if err := p.Write(ctx, "meter", bad.point, 1); err == nil {
			t.Errorf("wrote %s", bad.point)
		}
	}
	if err := p.Write(ctx, "nobody", "x", 1); err == nil {
		t.Error("wrote to an unknown device")
	}

	// Remove stops polling
	p.Remove("meter")
	before := p.Stats().Polls
	time.Sleep(300 * time.Millisecond)
	if after := p.Stats().Polls; after > before+1 {
		t.Fatalf("polls went on after Remove: %d -> %d", before, after)
	}
}

func TestPollerErrors(t *testing.T) {
	m := NewMemory(0, 0, 5, 0)
	addr := listen(t, NewServer(m, TCP).Handler())
	c := newClient(t, ClientConfig{})
	col := newCollector()
	p := NewPoller(PollerConfig{Interval: 100 * time.Millisecond, OnValues: col.on})
	defer p.Close()

	// One point beyond the device's table: its request fails, the others do not
	if err := p.Add(Device{Name: "partial", Link: LinkOf(c.Dial(addr)), Points: []Point{
		{Name: "ok", Address: 0}, {Name: "missing", Address: 100},
	}}); err != nil {
		t.Fatal(err)
	}
	col.wait(t, "partial")
	col.mu.Lock()
	ok, _ := valueOf(col.got["partial"], "ok")
	missing, _ := valueOf(col.got["partial"], "missing")
	col.mu.Unlock()
	if ok.Err != nil || !errors.Is(missing.Err, IllegalDataAddress) {
		t.Fatalf("ok %+v, missing %+v", ok, missing)
	}

	// A DTU that is not online: ErrOffline at once, nothing queued
	if err := p.Add(Device{Name: "dtu", Link: func() *Link { return nil }, Points: []Point{{Name: "x"}}}); err != nil {
		t.Fatal(err)
	}
	col.wait(t, "dtu")
	col.mu.Lock()
	x, _ := valueOf(col.got["dtu"], "x")
	col.mu.Unlock()
	if !errors.Is(x.Err, ErrOffline) {
		t.Fatalf("%+v", x)
	}
	if err := p.Write(ctxT(t), "dtu", "x", 1); !errors.Is(err, ErrOffline) {
		t.Fatalf("write to offline device: %v", err)
	}

	// Invalid devices
	for _, d := range []Device{
		{Name: "partial", Link: LinkOf(nil), Points: []Point{{Name: "a"}}},
		{Name: "a/b", Link: LinkOf(nil), Points: []Point{{Name: "a"}}},
		{Name: "dup", Link: LinkOf(nil), Points: []Point{{Name: "a"}, {Name: "a", Address: 1}}},
		{Name: "none", Link: LinkOf(nil)},
		{Name: "nolink", Points: []Point{{Name: "a"}}},
	} {
		if err := p.Add(d); err == nil {
			t.Errorf("added %+v", d)
		}
	}
}

// A device slower than its interval: polls do not pile up.
func TestPollerOverrun(t *testing.T) {
	d := newDevice(t, false, func(d *device, c net.Conn) {
		var pending []byte
		for {
			tid, unit, pdu, err := d.readRequest(c, &pending)
			if err != nil {
				return
			}
			time.Sleep(150 * time.Millisecond)
			c.Write(d.response(tid, unit, []byte{pdu[0], 2, 0, 1}))
		}
	})
	c := newClient(t, ClientConfig{})
	p := NewPoller(PollerConfig{Interval: 30 * time.Millisecond})
	defer p.Close()
	if err := p.Add(Device{Name: "slow", Link: LinkOf(c.Dial(d.addr())), Points: []Point{{Name: "x"}}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	st := p.Stats()
	if st.Overruns == 0 || st.Requests > 7 || st.Polls != st.Requests {
		t.Fatalf("stats %+v", st)
	}
}
