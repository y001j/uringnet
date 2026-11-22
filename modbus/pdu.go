// Package modbus implements Modbus TCP and Modbus RTU over TCP on uringnet:
//
//   - Client: a master that connects to devices and gateways, with pipelining (Modbus TCP), timeouts,
//     retries and automatic reconnection;
//   - DTUServer: a master for devices that connect in — 4G/Ethernet DTUs and serial device servers in
//     client mode — with registration and heartbeat packets, so thousands of field devices can be
//     polled over their own incoming connections;
//   - Server: a slave answering requests from a Handler, with Memory as a ready-made data model.
//
// All requests take a context. Requests on one Link are sent in order; Modbus RTU over TCP has no
// transaction IDs, so it sends one request at a time and waits briefly after a timeout before the
// next one, so a late response cannot be taken for the answer to another request.
package modbus

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Framing is how Modbus frames are carried over TCP.
type Framing int

const (
	// TCP is Modbus TCP: an MBAP header with a transaction ID, so several requests can be
	// outstanding on one connection.
	TCP Framing = iota
	// RTUOverTCP carries Modbus RTU frames (unit ID, PDU, CRC) over TCP, as serial device servers
	// and DTUs in transparent mode do. One request is outstanding at a time.
	RTUOverTCP
)

func (f Framing) String() string {
	if f == RTUOverTCP {
		return "RTU over TCP"
	}
	return "Modbus TCP"
}

// Function codes.
const (
	FuncReadCoils                  = 0x01
	FuncReadDiscreteInputs         = 0x02
	FuncReadHoldingRegisters       = 0x03
	FuncReadInputRegisters         = 0x04
	FuncWriteSingleCoil            = 0x05
	FuncWriteSingleRegister        = 0x06
	FuncWriteMultipleCoils         = 0x0F
	FuncWriteMultipleRegisters     = 0x10
	FuncReadWriteMultipleRegisters = 0x17
)

// Quantity limits of the Modbus specification.
const (
	MaxReadBits       = 2000
	MaxReadRegisters  = 125
	MaxWriteBits      = 1968
	MaxWriteRegisters = 123
	maxRWWrite        = 121 // registers written by function 0x17
)

// ExceptionCode is a Modbus exception code. A Handler returns one as an error to send that
// exception; a Client's error for an exception response unwraps to it, so
// errors.Is(err, modbus.IllegalDataAddress) works.
type ExceptionCode byte

const (
	IllegalFunction        ExceptionCode = 0x01
	IllegalDataAddress     ExceptionCode = 0x02
	IllegalDataValue       ExceptionCode = 0x03
	ServerDeviceFailure    ExceptionCode = 0x04
	Acknowledge            ExceptionCode = 0x05
	ServerDeviceBusy       ExceptionCode = 0x06
	MemoryParityError      ExceptionCode = 0x08
	GatewayPathUnavailable ExceptionCode = 0x0A
	GatewayTargetFailed    ExceptionCode = 0x0B
)

var exceptionNames = map[ExceptionCode]string{
	IllegalFunction: "illegal function", IllegalDataAddress: "illegal data address",
	IllegalDataValue: "illegal data value", ServerDeviceFailure: "server device failure",
	Acknowledge: "acknowledge", ServerDeviceBusy: "server device busy", MemoryParityError: "memory parity error",
	GatewayPathUnavailable: "gateway path unavailable", GatewayTargetFailed: "gateway target device failed to respond",
}

func (c ExceptionCode) Error() string {
	if n, ok := exceptionNames[c]; ok {
		return "modbus: " + n
	}
	return fmt.Sprintf("modbus: exception %#02x", byte(c))
}

// Exception is the error for an exception response.
type Exception struct {
	Function byte // the request's function code
	Code     ExceptionCode
}

func (e *Exception) Error() string {
	return fmt.Sprintf("%v (function %#02x)", e.Code, e.Function)
}

// Unwrap returns the exception code.
func (e *Exception) Unwrap() error { return e.Code }

var (
	// ErrTimeout: no response within the timeout, after all retries.
	ErrTimeout = errors.New("modbus: request timed out")
	// ErrClosed: the Link was closed.
	ErrClosed = errors.New("modbus: link closed")
	// ErrConnLost: the connection was lost before the response (and no retry was left, or the
	// Link belongs to a DTU, whose connection does not come back).
	ErrConnLost = errors.New("modbus: connection lost before the response")
	// ErrInvalidResponse: the response does not match the request.
	ErrInvalidResponse = errors.New("modbus: invalid response")
	// ErrCRC: a response with a wrong CRC (RTU over TCP) was received and no valid one followed.
	ErrCRC = errors.New("modbus: CRC mismatch")
	// ErrQuantity: a request asks for no items or more than the specification allows.
	ErrQuantity = errors.New("modbus: quantity out of range")
	// ErrNoResponse is returned by a Handler to send no response (for example for a unit ID that
	// is not present behind a gateway).
	ErrNoResponse = errors.New("modbus: no response")
)

// Request PDUs.

func pduAddrQty(fc byte, a, b uint16) []byte {
	return []byte{fc, byte(a >> 8), byte(a), byte(b >> 8), byte(b)}
}

func pduWriteCoils(addr uint16, values []bool) []byte {
	n := (len(values) + 7) / 8
	pdu := append(pduAddrQty(FuncWriteMultipleCoils, addr, uint16(len(values))), byte(n))
	return append(pdu, packBits(values)...)
}

func pduWriteRegisters(addr uint16, values []uint16) []byte {
	pdu := append(pduAddrQty(FuncWriteMultipleRegisters, addr, uint16(len(values))), byte(2*len(values)))
	return appendRegisters(pdu, values)
}

func pduReadWrite(readAddr, readQty, writeAddr uint16, values []uint16) []byte {
	pdu := pduAddrQty(FuncReadWriteMultipleRegisters, readAddr, readQty)
	pdu = binary.BigEndian.AppendUint16(pdu, writeAddr)
	pdu = binary.BigEndian.AppendUint16(pdu, uint16(len(values)))
	pdu = append(pdu, byte(2*len(values)))
	return appendRegisters(pdu, values)
}

func packBits(values []bool) []byte {
	b := make([]byte, (len(values)+7)/8)
	for i, v := range values {
		if v {
			b[i/8] |= 1 << (i % 8)
		}
	}
	return b
}

func unpackBits(b []byte, n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = b[i/8]&(1<<(i%8)) != 0
	}
	return out
}

func appendRegisters(dst []byte, values []uint16) []byte {
	for _, v := range values {
		dst = binary.BigEndian.AppendUint16(dst, v)
	}
	return dst
}

func registers(b []byte) []uint16 {
	out := make([]uint16, len(b)/2)
	for i := range out {
		out[i] = binary.BigEndian.Uint16(b[2*i:])
	}
	return out
}

// Response PDUs, checked against their request.

// checkResponse verifies that resp answers req: the same function code, or an exception for it.
func checkResponse(req, resp []byte) error {
	switch {
	case len(resp) == 2 && resp[0] == req[0]|0x80:
		return &Exception{Function: req[0], Code: ExceptionCode(resp[1])}
	case len(resp) == 0 || resp[0] != req[0]:
		return fmt.Errorf("%w: function %#02x for request %#02x", ErrInvalidResponse, firstByte(resp), req[0])
	}
	return nil
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}

// decodeBits decodes the response to a read coils/discrete inputs request.
func decodeBits(req, resp []byte) ([]bool, error) {
	qty := int(binary.BigEndian.Uint16(req[3:]))
	n := (qty + 7) / 8
	if len(resp) != 2+n || int(resp[1]) != n {
		return nil, fmt.Errorf("%w: %d bytes for %d bits", ErrInvalidResponse, len(resp), qty)
	}
	return unpackBits(resp[2:], qty), nil
}

// decodeRegisters decodes the response to a read registers request (0x03, 0x04 and 0x17, all of
// which have the read quantity at offset 3).
func decodeRegisters(req, resp []byte) ([]uint16, error) {
	qty := int(binary.BigEndian.Uint16(req[3:]))
	if len(resp) != 2+2*qty || int(resp[1]) != 2*qty {
		return nil, fmt.Errorf("%w: %d bytes for %d registers", ErrInvalidResponse, len(resp), qty)
	}
	return registers(resp[2:]), nil
}

// plausible reports whether resp can be the response to req: an exception for its function, or a
// response of the length (reads) or content (writes) the request calls for. RTU over TCP uses it to
// recognize a late response to an earlier request.
func plausible(req, resp []byte) bool {
	if checkResponse(req, resp) != nil {
		var e *Exception
		return errors.As(checkResponse(req, resp), &e)
	}
	var err error
	switch req[0] {
	case FuncReadCoils, FuncReadDiscreteInputs:
		_, err = decodeBits(req, resp)
	case FuncReadHoldingRegisters, FuncReadInputRegisters, FuncReadWriteMultipleRegisters:
		_, err = decodeRegisters(req, resp)
	case FuncWriteSingleCoil, FuncWriteSingleRegister, FuncWriteMultipleCoils, FuncWriteMultipleRegisters:
		err = checkEcho(req, resp)
	}
	return err == nil
}

// checkEcho verifies the response to a write: the request echoed (single writes) or its address
// and quantity (multiple writes).
func checkEcho(req, resp []byte) error {
	want := req
	if req[0] == FuncWriteMultipleCoils || req[0] == FuncWriteMultipleRegisters {
		want = req[:5]
	}
	if string(resp) != string(want) {
		return fmt.Errorf("%w: write response % x", ErrInvalidResponse, resp)
	}
	return nil
}

// ADUs: MBAP header + unit + PDU (Modbus TCP), unit + PDU + CRC (RTU).

const mbapLen = 7

func appendMBAP(dst []byte, tid uint16, unit byte, pdu []byte) []byte {
	dst = binary.BigEndian.AppendUint16(dst, tid)
	dst = append(dst, 0, 0) // protocol ID
	dst = binary.BigEndian.AppendUint16(dst, uint16(1+len(pdu)))
	dst = append(dst, unit)
	return append(dst, pdu...)
}

func appendRTU(dst []byte, unit byte, pdu []byte) []byte {
	start := len(dst)
	dst = append(dst, unit)
	dst = append(dst, pdu...)
	return binary.LittleEndian.AppendUint16(dst, crc16(dst[start:]))
}

// checkCRC reports whether an RTU frame ends with the CRC of its other bytes.
func checkCRC(frame []byte) bool {
	n := len(frame)
	return n >= 4 && crc16(frame[:n-2]) == binary.LittleEndian.Uint16(frame[n-2:])
}

var crcTable = func() (t [256]uint16) {
	for i := range t {
		c := uint16(i)
		for k := 0; k < 8; k++ {
			if c&1 != 0 {
				c = c>>1 ^ 0xA001
			} else {
				c >>= 1
			}
		}
		t[i] = c
	}
	return
}()

// crc16 is the Modbus CRC (CRC-16/MODBUS).
func crc16(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, c := range b {
		crc = crc>>8 ^ crcTable[byte(crc)^c]
	}
	return crc
}

// Framing of a byte stream. Each function returns the length of the frame at the start of buf,
// 0 if more bytes are needed, or -1 if buf does not start with a valid frame.

const maxRTUFrame = 256

// tcpFrameLen frames Modbus TCP: the MBAP length field counts the unit ID and the PDU.
func tcpFrameLen(buf []byte) int {
	if len(buf) < mbapLen {
		return 0
	}
	l := int(binary.BigEndian.Uint16(buf[4:]))
	if buf[2] != 0 || buf[3] != 0 || l < 2 || l > 254 {
		return -1
	}
	if len(buf) < 6+l {
		return 0
	}
	return 6 + l
}

// rtuFrameLen frames RTU, which has no length field: the length follows from the function code
// and, for variable-length frames, a byte count. Requests and responses differ.
func rtuFrameLen(buf []byte, request bool) int {
	if len(buf) < 2 {
		return 0
	}
	fc := buf[1]
	need := func(countAt, fixed int) int { // fixed bytes + the byte count found at countAt
		if len(buf) <= countAt {
			return 0
		}
		return fixed + int(buf[countAt])
	}
	n := -1
	switch {
	case !request && fc&0x80 != 0 && fc&0x7f != 0:
		n = 5 // unit, function, exception code, CRC
	case !request && (fc >= FuncReadCoils && fc <= FuncReadInputRegisters || fc == FuncReadWriteMultipleRegisters):
		n = need(2, 5)
	case fc >= FuncReadCoils && fc <= FuncWriteSingleRegister, fc == FuncWriteMultipleCoils, fc == FuncWriteMultipleRegisters:
		n = 8 // requests 0x01-0x06, responses 0x05, 0x06, 0x0F, 0x10
		if request && (fc == FuncWriteMultipleCoils || fc == FuncWriteMultipleRegisters) {
			n = need(6, 9)
		}
	case request && fc == FuncReadWriteMultipleRegisters:
		n = need(10, 13)
	}
	switch {
	case n == 0:
		return 0
	case n < 0 || n > maxRTUFrame:
		return -1
	case len(buf) < n:
		return 0
	}
	return n
}
