package modbus

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPointDecode(t *testing.T) {
	// 0x41C80000 is float32 25.0; 0x12345678 as integers
	for _, tc := range []struct {
		typ   DataType
		order ByteOrder
		regs  []uint16
		want  any
	}{
		{Uint16, ABCD, []uint16{0xFFFE}, 65534.0},
		{Int16, ABCD, []uint16{0xFFFE}, -2.0},
		{Int16, BADC, []uint16{0xFEFF}, -2.0},
		{Float32, ABCD, []uint16{0x41C8, 0x0000}, 25.0},
		{Float32, CDAB, []uint16{0x0000, 0x41C8}, 25.0},
		{Float32, BADC, []uint16{0xC841, 0x0000}, 25.0},
		{Float32, DCBA, []uint16{0x0000, 0xC841}, 25.0},
		{Uint32, ABCD, []uint16{0x1234, 0x5678}, float64(0x12345678)},
		{Uint32, CDAB, []uint16{0x5678, 0x1234}, float64(0x12345678)},
		{Int32, ABCD, []uint16{0xFFFF, 0xFFFF}, -1.0},
		{Int64, ABCD, []uint16{0xFFFF, 0xFFFF, 0xFFFF, 0xFFFE}, -2.0},
		{Uint64, CDAB, []uint16{0x0001, 0, 0, 0}, 1.0},
		{Float64, ABCD, []uint16{0x4039, 0, 0, 0}, 25.0},
		{Float64, DCBA, []uint16{0, 0, 0, 0x3940}, 25.0},
	} {
		p := Point{Name: "x", Type: tc.typ, Order: tc.order}
		got, err := p.DecodeRegisters(tc.regs)
		if err != nil || got != tc.want {
			t.Errorf("%v %v % x: %v %v, want %v", tc.typ, tc.order, tc.regs, got, err, tc.want)
		}
		// Encoding gives the registers back
		regs, err := p.EncodeRegisters(tc.want)
		if err != nil || !reflect.DeepEqual(regs, tc.regs) {
			t.Errorf("encode %v %v %v: % x %v, want % x", tc.typ, tc.order, tc.want, regs, err, tc.regs)
		}
	}

	// Scale and offset: raw 2305 * 0.1 = 230.5 V; -40 offset for a temperature
	v := Point{Name: "v", Scale: 0.1}
	if got, _ := v.DecodeRegisters([]uint16{2305}); got != 230.5 {
		t.Fatal(got)
	}
	if regs, err := v.EncodeRegisters(230.5); err != nil || regs[0] != 2305 {
		t.Fatal(regs, err)
	}
	// Decimal scales give the decimal, not 0.7000000000000001
	for raw, want := range map[uint16]float64{7: 0.7, 3: 0.3, 2301: 230.1, 1: 0.1} {
		if got, _ := v.DecodeRegisters([]uint16{raw}); got != want {
			t.Errorf("%d * 0.1 = %v", raw, got)
		}
	}
	cents := Point{Name: "c", Scale: 0.01}
	if got, _ := cents.DecodeRegisters([]uint16{2345}); got != 23.45 {
		t.Errorf("2345 * 0.01 = %v", got)
	}
	if regs, _ := cents.EncodeRegisters(23.45); regs[0] != 2345 {
		t.Errorf("23.45 / 0.01 = % x", regs)
	}
	temp := Point{Name: "t", Type: Int16, Offset: -40}
	if got, _ := temp.DecodeRegisters([]uint16{65}); got != 25.0 {
		t.Fatal(got)
	}

	// A bit in a register, and strings
	bit := Point{Name: "alarm", Type: Bool, Bit: 3}
	if got, _ := bit.DecodeRegisters([]uint16{0x0008}); got != true {
		t.Fatal(got)
	}
	s := Point{Name: "sn", Type: String, Length: 4}
	regs, err := s.EncodeRegisters("AB12C")
	if err != nil || !reflect.DeepEqual(regs, []uint16{0x4142, 0x3132, 0x4300, 0}) {
		t.Fatalf("% x %v", regs, err)
	}
	if got, _ := s.DecodeRegisters(regs); got != "AB12C" {
		t.Fatalf("%q", got)
	}
	s.Order = BADC
	if got, _ := s.DecodeRegisters([]uint16{0x4241, 0x3231, 0x0043, 0}); got != "AB12C" {
		t.Fatalf("byte-swapped string %q", got)
	}

	// Write values from MQTT/JSON: numbers of any type, numeric strings, json.Number, rounding, ranges
	u := Point{Name: "u"}
	for _, in := range []any{42, int64(42), uint8(42), 42.4, "42", json.Number("42"), float32(41.6)} {
		if regs, err := u.EncodeRegisters(in); err != nil || regs[0] != 42 {
			t.Errorf("%T %v: % x %v", in, in, regs, err)
		}
	}
	for _, bad := range []any{-1, 65536, "abc", []int{1}, math.NaN()} {
		if _, err := u.EncodeRegisters(bad); err == nil {
			t.Errorf("encoded %v for uint16", bad)
		}
	}
	if _, err := (&Point{Name: "i", Table: InputRegister}).EncodeRegisters(1); err == nil {
		t.Error("input register writable")
	}
	if _, err := bit.EncodeRegisters(true); err == nil {
		t.Error("bit in register writable")
	}
	c := Point{Name: "c", Table: Coil, Type: Bool}
	for in, want := range map[any]bool{true: true, "on": true, "OFF": false, 1: true, 0.0: false, json.Number("1"): true} {
		if got, err := c.EncodeBool(in); err != nil || got != want {
			t.Errorf("coil %v: %v %v", in, got, err)
		}
	}
}

func TestPointJSONAndCheck(t *testing.T) {
	var pts []Point
	err := json.Unmarshal([]byte(`[
		{"name": "voltage", "table": "input", "address": 0, "type": "float32", "order": "CDAB", "interval": "500ms"},
		{"name": "relay", "table": "coil", "address": 3, "type": "bool"},
		{"name": "energy", "address": 100, "type": "uint32", "scale": 0.01}
	]`), &pts)
	if err != nil {
		t.Fatal(err)
	}
	if pts[0].Table != InputRegister || pts[0].Type != Float32 || pts[0].Order != CDAB || pts[0].Interval != 500*time.Millisecond ||
		pts[1].Table != Coil || pts[2].Table != HoldingRegister || pts[2].Scale != 0.01 {
		t.Fatalf("%+v", pts)
	}
	b, _ := json.Marshal(pts[0])
	if !strings.Contains(string(b), `"interval":"500ms"`) || !strings.Contains(string(b), `"order":"CDAB"`) {
		t.Fatalf("%s", b)
	}
	if err := json.Unmarshal([]byte(`{"name":"x","type":"float16"}`), new(Point)); err == nil {
		t.Fatal("unknown type accepted")
	}

	for _, p := range []Point{
		{Name: ""}, {Name: "a/b"}, {Name: "c", Table: Coil, Type: Uint16}, {Name: "b", Type: Bool, Bit: 16},
		{Name: "s", Type: String}, {Name: "f", Type: Float64, Address: 65533},
	} {
		if err := p.check(); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	for _, p := range pts {
		if err := p.check(); err != nil {
			t.Error(err)
		}
	}
}

func TestPlan(t *testing.T) {
	pts := []Point{
		{Name: "a", Address: 0},                // 0
		{Name: "b", Address: 1, Type: Float32}, // 1-2
		{Name: "c", Address: 3},                // 3: contiguous with a, b
		{Name: "d", Address: 10},               // gap of 6
		{Name: "e", Address: 1, Type: Uint16},  // overlaps b
		{Name: "f", Table: InputRegister},      // another table
		{Name: "g", Unit: 2},                   // another unit
		{Name: "h", Table: Coil, Type: Bool},   // bits
		{Name: "i", Table: Coil, Type: Bool, Address: 40},
		{Name: "j", Address: 200, Type: String, Length: 100}, // 200-299
		{Name: "k", Address: 299, Type: Uint32},              // 299-300, would exceed 125 with j? 101 regs: fits
		{Name: "l", Address: 320},                            // 300..320 > 125 from 200? 121: fits only with gap
	}
	ptrs := func() []*Point {
		ps := make([]*Point, len(pts))
		for i := range pts {
			ps[i] = &pts[i]
		}
		return ps
	}
	summary := func(bs []block) []string {
		var out []string
		for _, b := range bs {
			var names []string
			for _, p := range b.points {
				names = append(names, p.Name)
			}
			out = append(out, strings.Join([]string{b.table.String(), string(rune('0' + b.unit)), itoa(b.start), itoa(b.count), strings.Join(names, "")}, " "))
		}
		return out
	}
	got := summary(plan(ptrs(), 1, 0))
	want := []string{
		"holding 1 0 4 abec", "holding 1 10 1 d", "holding 1 200 101 jk", "holding 1 320 1 l",
		"input 1 0 1 f", "coil 1 0 1 h", "coil 1 40 1 i", "holding 2 0 1 g",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("contiguous plan\n got %q\nwant %q", got, want)
	}
	// With gaps allowed: d joins the first block, l joins j and k (121 registers), the coils merge
	got = summary(plan(ptrs(), 1, 20))
	want = []string{
		"holding 1 0 11 abecd", "holding 1 200 121 jkl",
		"input 1 0 1 f", "coil 1 0 41 hi", "holding 2 0 1 g",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan with gaps\n got %q\nwant %q", got, want)
	}
	// Never more than 125 registers per request
	var many []*Point
	for i := 0; i < 300; i++ {
		many = append(many, &Point{Name: itoa(uint16(i)), Address: uint16(i)})
	}
	for _, b := range plan(many, 1, 0) {
		if b.count > MaxReadRegisters {
			t.Fatalf("block of %d registers", b.count)
		}
	}
	if n := len(plan(many, 1, 0)); n != 3 {
		t.Fatalf("%d blocks for 300 registers", n)
	}
}

func itoa(n uint16) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
