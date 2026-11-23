package modbus

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Table is one of the four Modbus data tables.
type Table uint8

const (
	HoldingRegister Table = iota // the default
	InputRegister
	Coil
	DiscreteInput
)

var tableNames = []string{"holding", "input", "coil", "discrete"}

func (t Table) String() string {
	if int(t) < len(tableNames) {
		return tableNames[t]
	}
	return fmt.Sprintf("Table(%d)", t)
}

// MarshalText and UnmarshalText use "holding", "input", "coil" and "discrete".
func (t Table) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

func (t *Table) UnmarshalText(b []byte) error { return parseName(b, tableNames, (*uint8)(t), "table") }

func (t Table) bits() bool { return t == Coil || t == DiscreteInput }

// DataType is how a point's registers are interpreted.
type DataType uint8

const (
	Uint16 DataType = iota // the default
	Int16
	Uint32
	Int32
	Float32
	Uint64
	Int64
	Float64
	Bool   // a coil or discrete input, or bit Point.Bit of a register
	String // Point.Length registers of text, two bytes per register
)

var typeNames = []string{"uint16", "int16", "uint32", "int32", "float32", "uint64", "int64", "float64", "bool", "string"}

func (d DataType) String() string {
	if int(d) < len(typeNames) {
		return typeNames[d]
	}
	return fmt.Sprintf("DataType(%d)", d)
}

// MarshalText and UnmarshalText use the lower-case type names ("uint16", "float32", ...).
func (d DataType) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d *DataType) UnmarshalText(b []byte) error {
	return parseName(b, typeNames, (*uint8)(d), "data type")
}

// ByteOrder is the order of the bytes of a value spread over registers. Devices disagree: the
// Modbus specification only fixes big-endian bytes within one register.
//
//	ABCD  registers most significant first, bytes big-endian (the default; "big endian")
//	CDAB  registers least significant first, bytes big-endian ("word swap")
//	BADC  registers most significant first, bytes swapped in each register ("byte swap")
//	DCBA  registers least significant first, bytes swapped ("little endian")
//
// For 16-bit values only the byte swap matters; for strings, registers are always in order and
// BADC/DCBA swap the two characters of each register.
type ByteOrder uint8

const (
	ABCD ByteOrder = iota
	CDAB
	BADC
	DCBA
)

var orderNames = []string{"ABCD", "CDAB", "BADC", "DCBA"}

func (o ByteOrder) String() string {
	if int(o) < len(orderNames) {
		return orderNames[o]
	}
	return fmt.Sprintf("ByteOrder(%d)", o)
}

// MarshalText and UnmarshalText use "ABCD", "CDAB", "BADC" and "DCBA".
func (o ByteOrder) MarshalText() ([]byte, error) { return []byte(o.String()), nil }

func (o *ByteOrder) UnmarshalText(b []byte) error {
	return parseName(b, orderNames, (*uint8)(o), "byte order")
}

func (o ByteOrder) wordSwap() bool { return o == CDAB || o == DCBA }
func (o ByteOrder) byteSwap() bool { return o == BADC || o == DCBA }

func parseName(b []byte, names []string, dst *uint8, what string) error {
	for i, n := range names {
		if strings.EqualFold(string(b), n) {
			*dst = uint8(i)
			return nil
		}
	}
	return fmt.Errorf("modbus: unknown %s %q (one of %s)", what, b, strings.Join(names, ", "))
}

// Point is a named value of a device: where it is and how to decode it. Points can be loaded from
// JSON, for example
//
//	{"name": "voltage", "table": "input", "address": 0, "type": "float32", "order": "CDAB", "scale": 1, "interval": "5s"}
type Point struct {
	Name string `json:"name"`
	// Unit is the unit ID; 0 uses the device's.
	Unit    byte      `json:"unit,omitempty"`
	Table   Table     `json:"table"`
	Address uint16    `json:"address"`
	Type    DataType  `json:"type"`
	Order   ByteOrder `json:"order"`
	// Bit selects the bit (0 = least significant) of a Bool point in a register table.
	Bit int `json:"bit,omitempty"`
	// Length is the number of registers of a String point.
	Length int `json:"length,omitempty"`
	// Scale and Offset convert numbers: value = raw*Scale + Offset (Scale 0 means 1).
	Scale  float64 `json:"scale,omitempty"`
	Offset float64 `json:"offset,omitempty"`
	// Interval is how often the point is read; 0 uses the poller's interval. In JSON it is a
	// duration string such as "500ms" or "1m".
	Interval time.Duration `json:"-"`
}

// pointJSON is Point's JSON form with Interval as a duration string.
type pointJSON struct {
	pointAlias
	Interval string `json:"interval,omitempty"`
}

type pointAlias Point

// MarshalJSON writes Interval as a duration string.
func (p Point) MarshalJSON() ([]byte, error) {
	j := pointJSON{pointAlias: pointAlias(p)}
	if p.Interval > 0 {
		j.Interval = p.Interval.String()
	}
	return json.Marshal(j)
}

// UnmarshalJSON reads Interval as a duration string.
func (p *Point) UnmarshalJSON(b []byte) error {
	var j pointJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*p = Point(j.pointAlias)
	if j.Interval != "" {
		d, err := time.ParseDuration(j.Interval)
		if err != nil {
			return fmt.Errorf("modbus: point %q: interval: %w", p.Name, err)
		}
		p.Interval = d
	}
	return nil
}

// size returns how many registers (or bits, in a bit table) the point occupies.
func (p *Point) size() int {
	switch p.Type {
	case Uint32, Int32, Float32:
		return 2
	case Uint64, Int64, Float64:
		return 4
	case String:
		return p.Length
	}
	return 1
}

func validName(s string) bool { return s != "" && !strings.ContainsAny(s, "/+#") }

// check validates a point.
func (p *Point) check() error {
	switch {
	case !validName(p.Name):
		return fmt.Errorf("modbus: invalid point name %q (empty, or contains / + #)", p.Name)
	case p.Table > DiscreteInput || p.Type > String || p.Order > DCBA:
		return fmt.Errorf("modbus: point %q: invalid table, type or byte order", p.Name)
	case p.Table.bits() && p.Type != Bool:
		return fmt.Errorf("modbus: point %q: %s points are bool", p.Name, p.Table)
	case p.Type == Bool && !p.Table.bits() && (p.Bit < 0 || p.Bit > 15):
		return fmt.Errorf("modbus: point %q: bit %d (0 to 15)", p.Name, p.Bit)
	case p.Type == String && (p.Length < 1 || p.Length > MaxReadRegisters):
		return fmt.Errorf("modbus: point %q: string length %d registers (1 to %d)", p.Name, p.Length, MaxReadRegisters)
	case int(p.Address)+p.size() > 0x10000:
		return fmt.Errorf("modbus: point %q: past address 65535", p.Name)
	case p.Interval < 0:
		return fmt.Errorf("modbus: point %q: negative interval", p.Name)
	}
	return nil
}

func (p *Point) scale() float64 {
	if p.Scale == 0 {
		return 1
	}
	return p.Scale
}

// regBytes lays registers out as big-endian bytes in value order (most significant first).
func (p *Point) regBytes(regs []uint16) []byte {
	b := make([]byte, 2*len(regs))
	for i, r := range regs {
		j := i
		if p.Order.wordSwap() && p.Type != String {
			j = len(regs) - 1 - i
		}
		if p.Order.byteSwap() {
			r = r<<8 | r>>8
		}
		binary.BigEndian.PutUint16(b[2*j:], r)
	}
	return b
}

// fromBytes is the inverse of regBytes.
func (p *Point) fromBytes(b []byte) []uint16 {
	regs := make([]uint16, len(b)/2)
	for i := range regs {
		j := i
		if p.Order.wordSwap() && p.Type != String {
			j = len(regs) - 1 - i
		}
		r := binary.BigEndian.Uint16(b[2*j:])
		if p.Order.byteSwap() {
			r = r<<8 | r>>8
		}
		regs[i] = r
	}
	return regs
}

// DecodeRegisters decodes the point from its registers (len == the point's size): a float64 for
// numbers (scaled), a bool or a string.
func (p *Point) DecodeRegisters(regs []uint16) (any, error) {
	if p.Table.bits() || len(regs) != p.size() {
		return nil, fmt.Errorf("modbus: point %q: %d registers for %d", p.Name, len(regs), p.size())
	}
	if p.Type == Bool {
		return regs[0]&(1<<p.Bit) != 0, nil
	}
	b := p.regBytes(regs)
	var raw float64
	switch p.Type {
	case Uint16:
		raw = float64(binary.BigEndian.Uint16(b))
	case Int16:
		raw = float64(int16(binary.BigEndian.Uint16(b)))
	case Uint32:
		raw = float64(binary.BigEndian.Uint32(b))
	case Int32:
		raw = float64(int32(binary.BigEndian.Uint32(b)))
	case Float32:
		raw = float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
	case Uint64:
		raw = float64(binary.BigEndian.Uint64(b))
	case Int64:
		raw = float64(int64(binary.BigEndian.Uint64(b)))
	case Float64:
		raw = math.Float64frombits(binary.BigEndian.Uint64(b))
	case String:
		return strings.TrimRight(string(b), "\x00 "), nil
	}
	return scaleUp(raw, p.scale()) + p.Offset, nil
}

// divisor returns d when scale is 1/d for an integer d (0.1, 0.01, ...), else 0. Dividing by d
// gives the decimal one expects (7 * 0.1 is 0.7000000000000001, 7 / 10 is 0.7).
func divisor(scale float64) float64 {
	if scale <= 0 || scale >= 1 {
		return 0
	}
	inv := 1 / scale
	if d := math.Round(inv); math.Abs(inv-d) < 1e-9*d {
		return d
	}
	return 0
}

func scaleUp(raw, scale float64) float64 {
	if d := divisor(scale); d != 0 {
		return raw / d
	}
	return raw * scale
}

func scaleDown(v, scale float64) float64 {
	if d := divisor(scale); d != 0 {
		return v * d
	}
	return v / scale
}

// errNotWritable is returned for points that cannot be written.
var errNotWritable = errors.New("not writable")

// EncodeRegisters converts a value (a number of any Go numeric type or json.Number, a numeric
// string, a bool, or a string for String points) to the point's registers, applying Scale and
// Offset in reverse. Integer types are rounded and range-checked.
func (p *Point) EncodeRegisters(v any) ([]uint16, error) {
	fail := func(format string, args ...any) ([]uint16, error) {
		return nil, fmt.Errorf("modbus: point %q: "+format, append([]any{p.Name}, args...)...)
	}
	if p.Table != HoldingRegister {
		return fail("%s points are %v", p.Table, errNotWritable)
	}
	switch p.Type {
	case Bool:
		return fail("a bit in a register is %v (it would need a read-modify-write)", errNotWritable)
	case String:
		s, ok := v.(string)
		if !ok {
			return fail("want a string, got %T", v)
		}
		if len(s) > 2*p.Length {
			return fail("%d bytes do not fit in %d registers", len(s), p.Length)
		}
		b := make([]byte, 2*p.Length)
		copy(b, s)
		return p.fromBytes(b), nil
	}
	f, err := toFloat(v)
	if err != nil {
		return fail("%v", err)
	}
	raw := scaleDown(f-p.Offset, p.scale())
	if math.IsNaN(raw) || math.IsInf(raw, 0) {
		return fail("invalid value %v", f)
	}
	b := make([]byte, 2*p.size())
	intRange := func(min, max float64) (float64, bool) {
		r := math.Round(raw)
		return r, r >= min && r <= max
	}
	switch p.Type {
	case Uint16, Int16:
		min, max := 0.0, float64(math.MaxUint16)
		if p.Type == Int16 {
			min, max = math.MinInt16, math.MaxInt16
		}
		r, ok := intRange(min, max)
		if !ok {
			return fail("%v out of range for %v", f, p.Type)
		}
		binary.BigEndian.PutUint16(b, uint16(int64(r)))
	case Uint32, Int32:
		min, max := 0.0, float64(math.MaxUint32)
		if p.Type == Int32 {
			min, max = math.MinInt32, math.MaxInt32
		}
		r, ok := intRange(min, max)
		if !ok {
			return fail("%v out of range for %v", f, p.Type)
		}
		binary.BigEndian.PutUint32(b, uint32(int64(r)))
	case Uint64:
		r, ok := intRange(0, math.MaxUint64)
		if !ok || r >= math.MaxUint64 {
			return fail("%v out of range for %v", f, p.Type)
		}
		binary.BigEndian.PutUint64(b, uint64(r))
	case Int64:
		r, ok := intRange(math.MinInt64, math.MaxInt64)
		if !ok || r >= math.MaxInt64 {
			return fail("%v out of range for %v", f, p.Type)
		}
		binary.BigEndian.PutUint64(b, uint64(int64(r)))
	case Float32:
		if math.Abs(raw) > math.MaxFloat32 {
			return fail("%v out of range for %v", f, p.Type)
		}
		binary.BigEndian.PutUint32(b, math.Float32bits(float32(raw)))
	case Float64:
		binary.BigEndian.PutUint64(b, math.Float64bits(raw))
	}
	return p.fromBytes(b), nil
}

// EncodeBool converts a value to a coil state: a bool, a number (0 or not), or "true"/"false"/
// "on"/"off"/"1"/"0".
func (p *Point) EncodeBool(v any) (bool, error) {
	if p.Table != Coil {
		return false, fmt.Errorf("modbus: point %q: %s points are %w", p.Name, p.Table, errNotWritable)
	}
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "on", "1":
			return true, nil
		case "false", "off", "0":
			return false, nil
		}
		return false, fmt.Errorf("modbus: point %q: %q is not a bool", p.Name, x)
	}
	f, err := toFloat(v)
	if err != nil {
		return false, fmt.Errorf("modbus: point %q: %v", p.Name, err)
	}
	return f != 0, nil
}

func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int8:
		return float64(x), nil
	case int16:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case uint:
		return float64(x), nil
	case uint8:
		return float64(x), nil
	case uint16:
		return float64(x), nil
	case uint32:
		return float64(x), nil
	case uint64:
		return float64(x), nil
	case json.Number:
		return x.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(x), 64)
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("want a number, got %T", v)
}

// block is one read request covering several points.
type block struct {
	unit         byte
	table        Table
	start, count uint16
	points       []*Point
}

func (b *block) pdu() []byte {
	fc := map[Table]byte{HoldingRegister: FuncReadHoldingRegisters, InputRegister: FuncReadInputRegisters,
		Coil: FuncReadCoils, DiscreteInput: FuncReadDiscreteInputs}[b.table]
	return pduAddrQty(fc, b.start, b.count)
}

// plan groups points into as few read requests as possible: points of one unit and table whose
// addresses are contiguous (or apart by at most maxGap registers, 16*maxGap bits) share a request,
// within the specification's size limits. defaultUnit replaces unit 0.
func plan(points []*Point, defaultUnit byte, maxGap int) []block {
	type key struct {
		unit  byte
		table Table
	}
	groups := map[key][]*Point{}
	var keys []key
	for _, p := range points {
		k := key{p.Unit, p.Table}
		if k.unit == 0 {
			k.unit = defaultUnit
		}
		if groups[k] == nil {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], p)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].unit != keys[j].unit {
			return keys[i].unit < keys[j].unit
		}
		return keys[i].table < keys[j].table
	})
	var blocks []block
	for _, k := range keys {
		ps := groups[k]
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].Address < ps[j].Address })
		limit, gap := MaxReadRegisters, maxGap
		if k.table.bits() {
			limit, gap = MaxReadBits, 16*maxGap
		}
		var cur *block
		end := 0 // first address after the current block
		for _, p := range ps {
			a, e := int(p.Address), int(p.Address)+p.size()
			if cur != nil && a <= end+gap && max(e, end)-int(cur.start) <= limit {
				if e > end {
					end = e
				}
				cur.count = uint16(end - int(cur.start))
				cur.points = append(cur.points, p)
				continue
			}
			blocks = append(blocks, block{unit: k.unit, table: k.table, start: p.Address, count: uint16(e - a), points: []*Point{p}})
			cur, end = &blocks[len(blocks)-1], e
		}
	}
	return blocks
}
