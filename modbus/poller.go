//go:build linux
// +build linux

package modbus

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// ErrOffline: the device is unreachable — a DTU that is not online, or a Client Link that did not
// get a connection within the poll interval (or the write's context).
var ErrOffline = errors.New("modbus: device offline")

// Device is a device for a Poller: its points and how to reach it.
type Device struct {
	// Name identifies the device (no / + #, so it can be an MQTT topic level).
	Name string
	// Unit is the unit ID of points without their own (default 1).
	Unit byte
	// Link returns the device's Link, or nil while it is unreachable. For a Client:
	// modbus.LinkOf(client.Dial(addr)); for a DTU: func() *modbus.Link { return dtu.Link(id) }.
	Link func() *Link
	// Points are the device's points; names must be unique within the device.
	Points []Point
}

// LinkOf returns a Device.Link function for a fixed Link.
func LinkOf(l *Link) func() *Link { return func() *Link { return l } }

// Value is a point's value from one poll.
type Value struct {
	Point *Point
	// Value is a float64 (numbers, scaled), a bool or a string; nil when Err is set.
	Value any
	Err   error
	Time  time.Time
}

// PollerConfig configures a Poller.
type PollerConfig struct {
	// Interval is how often points without their own interval are read (default 1s).
	Interval time.Duration
	// MaxGap lets one request span points up to MaxGap registers apart (16*MaxGap bits), reading
	// the registers in between. The default 0 only merges contiguous points: many devices answer
	// IllegalDataAddress for addresses they do not implement.
	MaxGap int
	// OnValues receives the values of each poll: the points of one device sharing an interval.
	// Calls come from one goroutine, one at a time, in order; they should return quickly.
	OnValues func(device string, values []Value)
}

// PollerStats are a Poller's counters.
type PollerStats struct {
	Polls    uint64 // polls of a device's interval group
	Requests uint64 // read requests sent
	Errors   uint64 // read requests that failed (offline devices included)
	Overruns uint64 // polls skipped because the previous one had not finished
}

// Poller reads devices' points periodically, merging adjacent points into one request, and
// reports their decoded values. Requests are asynchronous, so one Poller can poll thousands of
// devices from a single scheduling goroutine.
type Poller struct {
	cfg PollerConfig

	mu      sync.Mutex
	devices map[string]*pollDevice
	queue   groupHeap
	wake    chan struct{}
	closed  bool

	out  chan delivery
	done chan struct{}

	polls, requests, errs, overruns atomic.Uint64
}

type pollDevice struct {
	d      Device
	points map[string]*Point
	groups []*group
	last   map[*Point]Value
	gone   bool
}

// group is a device's points sharing an interval, read with the planned blocks.
type group struct {
	dev      *pollDevice
	interval time.Duration
	blocks   []block
	next     time.Time
	running  atomic.Bool
	index    int // in the heap
}

type delivery struct {
	device string
	values []Value
}

// NewPoller creates a poller and starts its goroutines; Close stops them.
func NewPoller(cfg PollerConfig) *Poller {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.MaxGap < 0 {
		cfg.MaxGap = 0
	}
	p := &Poller{cfg: cfg, devices: make(map[string]*pollDevice), wake: make(chan struct{}, 1),
		out: make(chan delivery, 256), done: make(chan struct{})}
	go p.schedule()
	go p.deliver()
	return p
}

// Add starts polling a device. It fails if the name is taken or a point is invalid.
func (p *Poller) Add(d Device) error {
	if !validName(d.Name) {
		return fmt.Errorf("modbus: invalid device name %q (empty, or contains / + #)", d.Name)
	}
	if d.Link == nil {
		return fmt.Errorf("modbus: device %q has no Link function", d.Name)
	}
	if len(d.Points) == 0 {
		return fmt.Errorf("modbus: device %q has no points", d.Name)
	}
	if d.Unit == 0 {
		d.Unit = 1
	}
	pd := &pollDevice{d: d, points: make(map[string]*Point), last: make(map[*Point]Value)}
	pd.d.Points = append([]Point(nil), d.Points...)
	byInterval := map[time.Duration][]*Point{}
	var intervals []time.Duration
	for i := range pd.d.Points {
		pt := &pd.d.Points[i]
		if err := pt.check(); err != nil {
			return fmt.Errorf("device %q: %w", d.Name, err)
		}
		if pd.points[pt.Name] != nil {
			return fmt.Errorf("modbus: device %q: duplicate point %q", d.Name, pt.Name)
		}
		pd.points[pt.Name] = pt
		iv := pt.Interval
		if iv == 0 {
			iv = p.cfg.Interval
		}
		if byInterval[iv] == nil {
			intervals = append(intervals, iv)
		}
		byInterval[iv] = append(byInterval[iv], pt)
	}
	now := time.Now()
	for _, iv := range intervals {
		// Spread first polls over up to a second, so thousands of devices added together (or at
		// start-up) do not send their requests at the same instant
		spread := iv
		if spread > time.Second {
			spread = time.Second
		}
		first := now.Add(time.Duration(rand.Int63n(int64(spread))))
		pd.groups = append(pd.groups, &group{dev: pd, interval: iv, blocks: plan(byInterval[iv], d.Unit, p.cfg.MaxGap), next: first})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("modbus: poller closed")
	}
	if p.devices[d.Name] != nil {
		return fmt.Errorf("modbus: device %q already added", d.Name)
	}
	p.devices[d.Name] = pd
	for _, g := range pd.groups {
		heap.Push(&p.queue, g)
	}
	p.signal()
	return nil
}

// Remove stops polling a device.
func (p *Poller) Remove(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.devices[name]
	if pd == nil {
		return
	}
	delete(p.devices, name)
	pd.gone = true
	for _, g := range pd.groups {
		if g.index >= 0 {
			heap.Remove(&p.queue, g.index)
		}
	}
}

// Devices returns the names of the devices.
func (p *Poller) Devices() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.devices))
	for n := range p.devices {
		names = append(names, n)
	}
	return names
}

// Last returns the latest value of each point of a device that has been polled.
func (p *Poller) Last(device string) []Value {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.devices[device]
	if pd == nil {
		return nil
	}
	out := make([]Value, 0, len(pd.last))
	for i := range pd.d.Points {
		if v, ok := pd.last[&pd.d.Points[i]]; ok {
			out = append(out, v)
		}
	}
	return out
}

// Stats returns the poller's counters.
func (p *Poller) Stats() PollerStats {
	return PollerStats{Polls: p.polls.Load(), Requests: p.requests.Load(), Errors: p.errs.Load(), Overruns: p.overruns.Load()}
}

// Close stops polling. Polls in progress finish, but their values are not delivered.
func (p *Poller) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	close(p.done)
}

func (p *Poller) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// schedule runs due groups.
func (p *Poller) schedule() {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		p.mu.Lock()
		now := time.Now()
		var due []*group
		for p.queue.Len() > 0 && !p.queue[0].next.After(now) {
			g := p.queue[0]
			due = append(due, g)
			// Next run on the interval grid; after a long stall, start again from now
			g.next = g.next.Add(g.interval)
			if g.next.Before(now) {
				g.next = now.Add(g.interval)
			}
			heap.Fix(&p.queue, 0)
		}
		wait := time.Hour
		if p.queue.Len() > 0 {
			wait = time.Until(p.queue[0].next)
		}
		p.mu.Unlock()
		for _, g := range due {
			p.run(g)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case <-p.done:
			return
		case <-p.wake:
		case <-timer.C:
		}
	}
}

// run polls a group: one asynchronous request per block, values delivered when all are done.
func (p *Poller) run(g *group) {
	if !g.running.CompareAndSwap(false, true) {
		p.overruns.Add(1)
		return
	}
	p.polls.Add(1)
	pd := g.dev
	values := make([]Value, 0, len(g.blocks))
	var mu sync.Mutex
	pending := int32(len(g.blocks))
	finish := func(vs []Value) {
		mu.Lock()
		values = append(values, vs...)
		mu.Unlock()
		if atomic.AddInt32(&pending, -1) > 0 {
			return
		}
		g.running.Store(false)
		p.record(pd, values)
	}
	l := pd.d.Link()
	if l == nil { // a DTU that is not online
		for i := range g.blocks {
			p.errs.Add(1)
			finish(failed(&g.blocks[i], ErrOffline))
		}
		return
	}
	// A Link that is (re)connecting gets the requests too: they wait for the connection, bounded
	// by the poll's deadline, and at most one poll per group is in progress, so nothing piles up.
	// Bound the poll by its interval (at least the link's timeout, so one attempt can finish)
	limit := g.interval
	if limit < l.cfg.Timeout {
		limit = l.cfg.Timeout
	}
	ctx, cancel := pollContext(limit) // cancelled by the last request's callback
	var left atomic.Int32
	left.Store(int32(len(g.blocks)))
	for i := range g.blocks {
		b := &g.blocks[i]
		req := b.pdu()
		p.requests.Add(1)
		l.Go(ctx, b.unit, req, func(resp []byte, err error) {
			if left.Add(-1) == 0 {
				cancel()
			}
			if err == nil {
				err = checkResponse(req, resp)
			}
			if err != nil {
				p.errs.Add(1)
				finish(failed(b, offline(l, err)))
				return
			}
			finish(decodeBlock(b, req, resp))
		})
	}
}

// pollContext bounds the requests of one poll; the caller cancels it when they are all done.
func pollContext(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// offline reports a request that ran out of time while the Link had no connection as ErrOffline.
func offline(l *Link, err error) error {
	if (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) && !l.Connected() {
		return ErrOffline
	}
	return err
}

func failed(b *block, err error) []Value {
	now := time.Now()
	vs := make([]Value, len(b.points))
	for i, pt := range b.points {
		vs[i] = Value{Point: pt, Err: err, Time: now}
	}
	return vs
}

// decodeBlock decodes the points of a block from its response.
func decodeBlock(b *block, req, resp []byte) []Value {
	now := time.Now()
	vs := make([]Value, len(b.points))
	if b.table.bits() {
		bits, err := decodeBits(req, resp)
		for i, pt := range b.points {
			vs[i] = Value{Point: pt, Err: err, Time: now}
			if err == nil {
				vs[i].Value = bits[pt.Address-b.start]
			}
		}
		return vs
	}
	regs, err := decodeRegisters(req, resp)
	for i, pt := range b.points {
		vs[i] = Value{Point: pt, Err: err, Time: now}
		if err == nil {
			off := int(pt.Address - b.start)
			vs[i].Value, vs[i].Err = pt.DecodeRegisters(regs[off : off+pt.size()])
		}
	}
	return vs
}

// record stores a poll's values and hands them to the delivery goroutine, in point order.
func (p *Poller) record(pd *pollDevice, values []Value) {
	order := make(map[*Point]int, len(pd.d.Points))
	for i := range pd.d.Points {
		order[&pd.d.Points[i]] = i
	}
	sortValues(values, order)
	p.mu.Lock()
	if pd.gone || p.closed {
		p.mu.Unlock()
		return
	}
	for _, v := range values {
		pd.last[v.Point] = v
	}
	p.mu.Unlock()
	if p.cfg.OnValues == nil {
		return
	}
	select {
	case p.out <- delivery{pd.d.Name, values}:
	case <-p.done:
	}
}

func sortValues(vs []Value, order map[*Point]int) {
	for i := 1; i < len(vs); i++ {
		for j := i; j > 0 && order[vs[j].Point] < order[vs[j-1].Point]; j-- {
			vs[j], vs[j-1] = vs[j-1], vs[j]
		}
	}
}

func (p *Poller) deliver() {
	for {
		select {
		case d := <-p.out:
			p.cfg.OnValues(d.device, d.values)
		case <-p.done:
			return
		}
	}
}

// Write writes a value to a point: a number or numeric string for registers (scaled and encoded
// by the point's type and byte order), a string for String points, a bool for coils. Single
// registers and coils use functions 0x06 and 0x05, longer values 0x10.
func (p *Poller) Write(ctx context.Context, device, point string, v any) error {
	ch := make(chan error, 1)
	p.WriteAsync(ctx, device, point, v, func(err error) { ch <- err })
	return <-ch
}

// WriteAsync is Write without blocking: done (which must not block) receives the result.
func (p *Poller) WriteAsync(ctx context.Context, device, point string, v any, done func(error)) {
	p.mu.Lock()
	pd := p.devices[device]
	var pt *Point
	if pd != nil {
		pt = pd.points[point]
	}
	p.mu.Unlock()
	switch {
	case pd == nil:
		done(fmt.Errorf("modbus: unknown device %q", device))
		return
	case pt == nil:
		done(fmt.Errorf("modbus: device %q has no point %q", device, point))
		return
	}
	var req []byte
	if pt.Table == Coil {
		on, err := pt.EncodeBool(v)
		if err != nil {
			done(err)
			return
		}
		val := uint16(0)
		if on {
			val = 0xFF00
		}
		req = pduAddrQty(FuncWriteSingleCoil, pt.Address, val)
	} else {
		regs, err := pt.EncodeRegisters(v)
		if err != nil {
			done(err)
			return
		}
		if len(regs) == 1 {
			req = pduAddrQty(FuncWriteSingleRegister, pt.Address, regs[0])
		} else {
			req = pduWriteRegisters(pt.Address, regs)
		}
	}
	l := pd.d.Link()
	if l == nil {
		done(ErrOffline)
		return
	}
	unit := pt.Unit
	if unit == 0 {
		unit = pd.d.Unit
	}
	l.Go(ctx, unit, req, func(resp []byte, err error) {
		if err == nil {
			err = checkResponse(req, resp)
		}
		if err == nil {
			err = checkEcho(req, resp)
		}
		done(offline(l, err))
	})
}

// groupHeap orders groups by their next run.
type groupHeap []*group

func (h groupHeap) Len() int           { return len(h) }
func (h groupHeap) Less(i, j int) bool { return h[i].next.Before(h[j].next) }
func (h groupHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *groupHeap) Push(x any) {
	g := x.(*group)
	g.index = len(*h)
	*h = append(*h, g)
}
func (h *groupHeap) Pop() any {
	old := *h
	g := old[len(old)-1]
	old[len(old)-1] = nil
	g.index = -1
	*h = old[:len(old)-1]
	return g
}
