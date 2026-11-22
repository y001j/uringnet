package modbus

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(string(bytes.ReplaceAll([]byte(s), []byte(" "), nil)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCRC(t *testing.T) {
	// Well-known frames from the Modbus over serial line specification and device manuals
	for _, f := range []string{
		"01 03 00 00 00 0A C5 CD",
		"11 03 00 6B 00 03 76 87",
		"01 06 00 01 00 03 98 0B",
	} {
		frame := unhex(t, f)
		if !checkCRC(frame) {
			t.Errorf("CRC of %s: got %04x", f, crc16(frame[:len(frame)-2]))
		}
		if got := appendRTU(nil, frame[0], frame[1:len(frame)-2]); !bytes.Equal(got, frame) {
			t.Errorf("appendRTU = % x, want %s", got, f)
		}
	}
	if checkCRC(unhex(t, "01 03 00 00 00 0A C5 CE")) {
		t.Error("wrong CRC accepted")
	}
}

func TestFrameLengths(t *testing.T) {
	for _, tc := range []struct {
		frame   string
		request bool
		want    int
	}{
		{"01 03 00 00 00 0A C5 CD", true, 8},
		{"01 01 00 00 00 0A 00 00", true, 8},
		{"01 06 00 01 00 03 98 0B", true, 8},
		{"01 10 00 01 00 02 04 00 0A 01 02 00 00", true, 13},
		{"01 0F 00 13 00 0A 02 CD 01 00 00", true, 11},
		{"01 17 00 03 00 06 00 0E 00 03 06 00 FF 00 FF 00 FF 00 00", true, 19},
		{"01 03 04 00 01 00 02 00 00", false, 9},
		{"01 01 01 05 00 00", false, 6},
		{"01 10 00 01 00 02 00 00", false, 8},
		{"01 83 02 00 00", false, 5},
		{"01 17 02 00 01 00 00", false, 7},
	} {
		frame := unhex(t, tc.frame)
		if got := rtuFrameLen(append(frame, 0xAA, 0xBB), tc.request); got != tc.want {
			t.Errorf("%s (request %v): %d, want %d", tc.frame, tc.request, got, tc.want)
		}
		for i := 0; i < len(frame); i++ {
			if got := rtuFrameLen(frame[:i], tc.request); got != 0 {
				t.Errorf("%s cut at %d: %d, want 0", tc.frame, i, got)
			}
		}
	}
	// Unknown function codes cannot be framed; exceptions are responses only
	for _, tc := range []struct {
		frame   string
		request bool
	}{{"01 41 00 00", true}, {"01 41 00 00", false}, {"01 83 02 00 00", true}, {"01 80 02 00 00", false}} {
		if got := rtuFrameLen(unhex(t, tc.frame), tc.request); got != -1 {
			t.Errorf("%s (request %v): %d, want -1", tc.frame, tc.request, got)
		}
	}

	adu := appendMBAP(nil, 0x1234, 7, []byte{3, 0, 0, 0, 2})
	if got := tcpFrameLen(append(adu, 0, 0)); got != len(adu) || len(adu) != 12 {
		t.Fatalf("MBAP frame %d of % x", got, adu)
	}
	for i := 0; i < len(adu); i++ {
		if got := tcpFrameLen(adu[:i]); got != 0 {
			t.Fatalf("MBAP cut at %d: %d", i, got)
		}
	}
	for _, bad := range []string{"00 01 00 01 00 06 01 03 00 00 00 01", "00 01 00 00 00 01 01", "00 01 00 00 01 00 01"} {
		if got := tcpFrameLen(unhex(t, bad)); got != -1 {
			t.Errorf("MBAP %s: %d, want -1", bad, got)
		}
	}
}

func TestServe(t *testing.T) {
	m := NewMemory(32, 16, 64, 8)
	m.SetDiscreteInputs(3, []bool{true, false, true})
	m.SetInputRegisters(0, []uint16{100, 200})
	var writes []string
	m.OnWrite = func(unit byte, coils bool, addr uint16, n int) {
		writes = append(writes, hex.EncodeToString([]byte{unit, map[bool]byte{true: 1}[coils], byte(addr), byte(n)}))
	}
	for _, tc := range []struct{ req, resp string }{
		{"05 0002 FF00", "05 0002 FF00"},                      // write single coil
		{"0F 0004 000A 02 CD01", "0F 0004 000A"},              // write coils 4..13
		{"01 0000 0010", "01 02 D41C"},                        // read coils 0..15
		{"02 0003 0003", "02 01 05"},                          // read discrete inputs
		{"06 0001 1234", "06 0001 1234"},                      // write single register
		{"10 0002 0002 04 000A 0102", "10 0002 0002"},         // write registers
		{"03 0001 0003", "03 06 1234 000A 0102"},              // read holding registers
		{"04 0000 0002", "04 04 0064 00C8"},                   // read input registers
		{"17 0000 0002 0000 0001 02 ABCD", "17 04 ABCD 1234"}, // write then read
		// Exceptions
		{"03 003F 0002", "83 02"},            // beyond the table
		{"03 0000 007E", "83 03"},            // more than 125 registers
		{"03 FFFF 0002", "83 02"},            // past address 65535
		{"01 0000 0000", "81 03"},            // zero quantity
		{"05 0000 1234", "85 03"},            // coil value other than FF00/0000
		{"10 0000 0002 03 000A 01", "90 03"}, // byte count does not match
		{"03 0000", "83 03"},                 // truncated
		{"41 00", "C1 01"},                   // unsupported function
	} {
		got := serve(m, 9, unhex(t, tc.req))
		if want := unhex(t, tc.resp); !bytes.Equal(got, want) {
			t.Errorf("%s: got % x, want %s", tc.req, got, tc.resp)
		}
	}
	if len(writes) != 5 || writes[0] != "09010201" || writes[2] != "09000101" {
		t.Errorf("OnWrite calls %v", writes)
	}
	coils, _ := m.Coils(0, 16)
	if !coils[2] || !coils[4] || coils[5] || !coils[6] || !coils[12] {
		t.Errorf("coils %v", coils)
	}

	// Units: other units get no response; handler errors become exceptions
	u := Units{1: m, 2: failing{NewMemory(1, 1, 1, 1)}}
	if resp := serve(u, 3, unhex(t, "03 0000 0001")); resp != nil {
		t.Errorf("absent unit answered % x", resp)
	}
	if resp := serve(u, 2, unhex(t, "03 0000 0001")); !bytes.Equal(resp, []byte{0x83, 0x04}) {
		t.Errorf("failing handler: % x", resp)
	}
	if resp := serve(u, 2, unhex(t, "01 0000 0001")); !bytes.Equal(resp, []byte{0x81, 0x0B}) {
		t.Errorf("gateway exception: % x", resp)
	}
}

type failing struct{ *Memory }

func (failing) ReadHoldingRegisters(byte, uint16, uint16) ([]uint16, error) {
	return nil, errors.New("sensor bus down")
}

func (failing) ReadCoils(byte, uint16, uint16) ([]bool, error) { return nil, GatewayTargetFailed }

func TestResponseDecoding(t *testing.T) {
	req := pduAddrQty(FuncReadHoldingRegisters, 0, 2)
	if regs, err := decodeRegisters(req, unhex(t, "03 04 0001 0002")); err != nil || regs[1] != 2 {
		t.Fatal(regs, err)
	}
	if _, err := decodeRegisters(req, unhex(t, "03 02 0001")); !errors.Is(err, ErrInvalidResponse) {
		t.Fatal(err)
	}
	err := checkResponse(req, []byte{0x83, 0x02})
	var e *Exception
	if !errors.As(err, &e) || e.Code != IllegalDataAddress || !errors.Is(err, IllegalDataAddress) {
		t.Fatalf("%v", err)
	}
	if err := checkResponse(req, []byte{0x04, 0}); !errors.Is(err, ErrInvalidResponse) {
		t.Fatal(err)
	}
	bits := []bool{true, false, true, true, false, false, false, false, true}
	if got := unpackBits(packBits(bits), len(bits)); len(got) != 9 || !got[0] || got[1] || !got[8] {
		t.Fatal(got)
	}
	w := pduWriteRegisters(5, []uint16{1, 2})
	if err := checkEcho(w, w[:5]); err != nil {
		t.Fatal(err)
	}
	if err := checkEcho(w, []byte{0x10, 0, 5, 0, 3}); err == nil {
		t.Fatal("wrong echo accepted")
	}
}
