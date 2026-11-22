package modbus

import (
	"encoding/binary"
	"errors"
	"sync"
)

// Handler answers the requests a Server receives. Methods run on the connection's event loop and
// must not block. Returning an ExceptionCode sends that exception, ErrNoResponse sends nothing,
// and any other error sends ServerDeviceFailure. Read methods must return exactly qty values.
type Handler interface {
	ReadCoils(unit byte, addr, qty uint16) ([]bool, error)
	ReadDiscreteInputs(unit byte, addr, qty uint16) ([]bool, error)
	ReadHoldingRegisters(unit byte, addr, qty uint16) ([]uint16, error)
	ReadInputRegisters(unit byte, addr, qty uint16) ([]uint16, error)
	WriteCoils(unit byte, addr uint16, values []bool) error
	WriteHoldingRegisters(unit byte, addr uint16, values []uint16) error
}

// serve executes a request PDU and returns the response PDU, nil for no response.
func serve(h Handler, unit byte, req []byte) []byte {
	if len(req) == 0 {
		return nil
	}
	fc := req[0]
	exception := func(c ExceptionCode) []byte { return []byte{fc | 0x80, byte(c)} }
	fail := func(err error) []byte {
		var c ExceptionCode
		switch {
		case errors.Is(err, ErrNoResponse):
			return nil
		case errors.As(err, &c):
			return exception(c)
		}
		return exception(ServerDeviceFailure)
	}
	u16 := func(i int) uint16 { return binary.BigEndian.Uint16(req[i:]) }
	// checkRange validates a quantity and that the addressed range fits in 16 bits
	checkRange := func(addr, qty uint16, max int) []byte {
		if qty < 1 || int(qty) > max {
			return exception(IllegalDataValue)
		}
		if int(addr)+int(qty) > 0x10000 {
			return exception(IllegalDataAddress)
		}
		return nil
	}
	switch fc {
	case FuncReadCoils, FuncReadDiscreteInputs:
		if len(req) != 5 {
			return exception(IllegalDataValue)
		}
		addr, qty := u16(1), u16(3)
		if e := checkRange(addr, qty, MaxReadBits); e != nil {
			return e
		}
		read := h.ReadCoils
		if fc == FuncReadDiscreteInputs {
			read = h.ReadDiscreteInputs
		}
		bits, err := read(unit, addr, qty)
		if err != nil {
			return fail(err)
		}
		if len(bits) != int(qty) {
			return exception(ServerDeviceFailure)
		}
		b := packBits(bits)
		return append([]byte{fc, byte(len(b))}, b...)

	case FuncReadHoldingRegisters, FuncReadInputRegisters:
		if len(req) != 5 {
			return exception(IllegalDataValue)
		}
		addr, qty := u16(1), u16(3)
		if e := checkRange(addr, qty, MaxReadRegisters); e != nil {
			return e
		}
		read := h.ReadHoldingRegisters
		if fc == FuncReadInputRegisters {
			read = h.ReadInputRegisters
		}
		regs, err := read(unit, addr, qty)
		if err != nil {
			return fail(err)
		}
		if len(regs) != int(qty) {
			return exception(ServerDeviceFailure)
		}
		return appendRegisters([]byte{fc, byte(2 * qty)}, regs)

	case FuncWriteSingleCoil:
		if len(req) != 5 {
			return exception(IllegalDataValue)
		}
		v := u16(3)
		if v != 0xFF00 && v != 0 {
			return exception(IllegalDataValue)
		}
		if err := h.WriteCoils(unit, u16(1), []bool{v == 0xFF00}); err != nil {
			return fail(err)
		}
		return append([]byte(nil), req...)

	case FuncWriteSingleRegister:
		if len(req) != 5 {
			return exception(IllegalDataValue)
		}
		if err := h.WriteHoldingRegisters(unit, u16(1), []uint16{u16(3)}); err != nil {
			return fail(err)
		}
		return append([]byte(nil), req...)

	case FuncWriteMultipleCoils:
		if len(req) < 6 {
			return exception(IllegalDataValue)
		}
		addr, qty, n := u16(1), u16(3), int(req[5])
		if e := checkRange(addr, qty, MaxWriteBits); e != nil {
			return e
		}
		if n != (int(qty)+7)/8 || len(req) != 6+n {
			return exception(IllegalDataValue)
		}
		if err := h.WriteCoils(unit, addr, unpackBits(req[6:], int(qty))); err != nil {
			return fail(err)
		}
		return append([]byte(nil), req[:5]...)

	case FuncWriteMultipleRegisters:
		if len(req) < 6 {
			return exception(IllegalDataValue)
		}
		addr, qty, n := u16(1), u16(3), int(req[5])
		if e := checkRange(addr, qty, MaxWriteRegisters); e != nil {
			return e
		}
		if n != 2*int(qty) || len(req) != 6+n {
			return exception(IllegalDataValue)
		}
		if err := h.WriteHoldingRegisters(unit, addr, registers(req[6:])); err != nil {
			return fail(err)
		}
		return append([]byte(nil), req[:5]...)

	case FuncReadWriteMultipleRegisters:
		if len(req) < 10 {
			return exception(IllegalDataValue)
		}
		raddr, rqty, waddr, wqty, n := u16(1), u16(3), u16(5), u16(7), int(req[9])
		if e := checkRange(raddr, rqty, MaxReadRegisters); e != nil {
			return e
		}
		if e := checkRange(waddr, wqty, maxRWWrite); e != nil {
			return e
		}
		if n != 2*int(wqty) || len(req) != 10+n {
			return exception(IllegalDataValue)
		}
		// The write is performed before the read (specification section 6.17)
		if err := h.WriteHoldingRegisters(unit, waddr, registers(req[10:])); err != nil {
			return fail(err)
		}
		regs, err := h.ReadHoldingRegisters(unit, raddr, rqty)
		if err != nil {
			return fail(err)
		}
		if len(regs) != int(rqty) {
			return exception(ServerDeviceFailure)
		}
		return appendRegisters([]byte{fc, byte(2 * rqty)}, regs)
	}
	return exception(IllegalFunction)
}

// Memory is a Handler that keeps the four Modbus tables in memory and answers every unit ID the
// same way. It is safe for concurrent use: the application updates inputs and reads what masters
// wrote with the Set and Get methods. Addresses outside the tables get IllegalDataAddress.
type Memory struct {
	mu                     sync.RWMutex
	coils, discreteInputs  []bool
	holding, inputRegister []uint16
	// OnWrite, if set, is called after a master writes coils (coils true) or holding registers,
	// with the address and the number of items written. It runs on an event loop; it must not
	// block.
	OnWrite func(unit byte, coils bool, addr uint16, n int)
}

// NewMemory creates a Memory with tables of the given sizes (at most 65536 each).
func NewMemory(coils, discreteInputs, holdingRegisters, inputRegisters int) *Memory {
	return &Memory{coils: make([]bool, coils), discreteInputs: make([]bool, discreteInputs),
		holding: make([]uint16, holdingRegisters), inputRegister: make([]uint16, inputRegisters)}
}

func get[T any](mu *sync.RWMutex, table []T, addr, qty uint16) ([]T, error) {
	mu.RLock()
	defer mu.RUnlock()
	if int(addr)+int(qty) > len(table) {
		return nil, IllegalDataAddress
	}
	return append([]T(nil), table[addr:int(addr)+int(qty)]...), nil
}

func set[T any](mu *sync.RWMutex, table []T, addr uint16, values []T) error {
	mu.Lock()
	defer mu.Unlock()
	if int(addr)+len(values) > len(table) {
		return IllegalDataAddress
	}
	copy(table[addr:], values)
	return nil
}

func (m *Memory) ReadCoils(_ byte, addr, qty uint16) ([]bool, error) {
	return get(&m.mu, m.coils, addr, qty)
}

func (m *Memory) ReadDiscreteInputs(_ byte, addr, qty uint16) ([]bool, error) {
	return get(&m.mu, m.discreteInputs, addr, qty)
}

func (m *Memory) ReadHoldingRegisters(_ byte, addr, qty uint16) ([]uint16, error) {
	return get(&m.mu, m.holding, addr, qty)
}

func (m *Memory) ReadInputRegisters(_ byte, addr, qty uint16) ([]uint16, error) {
	return get(&m.mu, m.inputRegister, addr, qty)
}

func (m *Memory) WriteCoils(unit byte, addr uint16, values []bool) error {
	if err := set(&m.mu, m.coils, addr, values); err != nil {
		return err
	}
	if m.OnWrite != nil {
		m.OnWrite(unit, true, addr, len(values))
	}
	return nil
}

func (m *Memory) WriteHoldingRegisters(unit byte, addr uint16, values []uint16) error {
	if err := set(&m.mu, m.holding, addr, values); err != nil {
		return err
	}
	if m.OnWrite != nil {
		m.OnWrite(unit, false, addr, len(values))
	}
	return nil
}

// SetCoils, SetDiscreteInputs, SetHoldingRegisters and SetInputRegisters update the tables from
// the application; they return IllegalDataAddress if the values do not fit.
func (m *Memory) SetCoils(addr uint16, v []bool) error { return set(&m.mu, m.coils, addr, v) }
func (m *Memory) SetDiscreteInputs(addr uint16, v []bool) error {
	return set(&m.mu, m.discreteInputs, addr, v)
}
func (m *Memory) SetHoldingRegisters(addr uint16, v []uint16) error {
	return set(&m.mu, m.holding, addr, v)
}
func (m *Memory) SetInputRegisters(addr uint16, v []uint16) error {
	return set(&m.mu, m.inputRegister, addr, v)
}

// Coils and HoldingRegisters read the writable tables back, for example after OnWrite.
func (m *Memory) Coils(addr, qty uint16) ([]bool, error) { return get(&m.mu, m.coils, addr, qty) }
func (m *Memory) HoldingRegisters(addr, qty uint16) ([]uint16, error) {
	return get(&m.mu, m.holding, addr, qty)
}

// Units routes requests by unit ID to one Handler per unit, as a gateway with several devices
// behind it does. Requests for other units get no response (Modbus RTU semantics); use a Handler
// returning GatewayTargetFailed instead to report them.
type Units map[byte]Handler

func (u Units) pick(unit byte) (Handler, error) {
	if h, ok := u[unit]; ok {
		return h, nil
	}
	return nil, ErrNoResponse
}

func (u Units) ReadCoils(unit byte, addr, qty uint16) ([]bool, error) {
	h, err := u.pick(unit)
	if err != nil {
		return nil, err
	}
	return h.ReadCoils(unit, addr, qty)
}

func (u Units) ReadDiscreteInputs(unit byte, addr, qty uint16) ([]bool, error) {
	h, err := u.pick(unit)
	if err != nil {
		return nil, err
	}
	return h.ReadDiscreteInputs(unit, addr, qty)
}

func (u Units) ReadHoldingRegisters(unit byte, addr, qty uint16) ([]uint16, error) {
	h, err := u.pick(unit)
	if err != nil {
		return nil, err
	}
	return h.ReadHoldingRegisters(unit, addr, qty)
}

func (u Units) ReadInputRegisters(unit byte, addr, qty uint16) ([]uint16, error) {
	h, err := u.pick(unit)
	if err != nil {
		return nil, err
	}
	return h.ReadInputRegisters(unit, addr, qty)
}

func (u Units) WriteCoils(unit byte, addr uint16, values []bool) error {
	h, err := u.pick(unit)
	if err != nil {
		return err
	}
	return h.WriteCoils(unit, addr, values)
}

func (u Units) WriteHoldingRegisters(unit byte, addr uint16, values []uint16) error {
	h, err := u.pick(unit)
	if err != nil {
		return err
	}
	return h.WriteHoldingRegisters(unit, addr, values)
}
