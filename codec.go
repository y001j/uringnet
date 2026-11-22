//go:build linux
// +build linux

package uringnet

import (
	"bytes"
	"encoding/binary"
	stderrors "errors"
	"fmt"
)

// ErrFrameTooLarge is passed to OnClose when a peer sends a frame larger than the
// framer allows; the connection is closed.
var ErrFrameTooLarge = stderrors.New("uringnet: frame too large")

// Framer splits a byte stream into frames.
type Framer interface {
	// Frame returns the length of the first complete frame at the start of buf, 0 if buf
	// does not hold a complete frame yet, or an error if buf can never start a valid frame
	// (the connection is then closed with that error). The returned length must not
	// exceed len(buf).
	Frame(buf []byte) (n int, err error)
}

// MessageHandler receives whole frames from a connection wrapped with Framed. The other
// methods are the same as in Handler; embed BuiltinHandler to get no-op versions.
type MessageHandler interface {
	OnBoot(s *Server)
	OnOpen(c *Conn) Action
	// OnMessage is called once per complete frame, including its header or delimiter.
	// msg is only valid until OnMessage returns. When the frame arrived in one piece msg
	// points into the receive buffer, so writing it back with c.Write sends it in place.
	OnMessage(c *Conn, msg []byte) Action
	OnClose(c *Conn, err error)
	OnShutdown(s *Server)
}

// Framed returns a Handler that splits each connection's byte stream into frames with f
// and calls h.OnMessage once per frame. Partial frames are kept per connection until the
// rest arrives; complete frames are passed straight from the receive buffer.
func Framed(f Framer, h MessageHandler) Handler { return framed{f: f, h: h} }

type framed struct {
	f Framer
	h MessageHandler
}

func (fh framed) OnBoot(s *Server)           { fh.h.OnBoot(s) }
func (fh framed) OnOpen(c *Conn) Action      { return fh.h.OnOpen(c) }
func (fh framed) OnClose(c *Conn, err error) { fh.h.OnClose(c, err); c.pending = nil }
func (fh framed) OnShutdown(s *Server)       { fh.h.OnShutdown(s) }

func (fh framed) OnTraffic(c *Conn, data []byte) Action {
	buf := data
	buffered := len(c.pending) > 0
	if buffered {
		c.pending = append(c.pending, data...)
		buf = c.pending
	}
	action := None
	for len(buf) > 0 && action == None && !c.c.closed && !c.c.closeAfter {
		n, err := fh.f.Frame(buf)
		if err != nil {
			c.c.fail(err)
			c.pending = nil
			return None
		}
		if n <= 0 {
			break
		}
		if n > len(buf) {
			c.c.fail(fmt.Errorf("uringnet: Framer returned %d for %d buffered bytes", n, len(buf)))
			c.pending = nil
			return None
		}
		action = fh.h.OnMessage(c, buf[:n])
		buf = buf[n:]
	}
	// 保留不完整的帧：buf 是 pending 的后缀时原地前移，否则（位于接收缓冲区）拷贝出来
	c.pending = append(c.pending[:0], buf...)
	if len(c.pending) == 0 && cap(c.pending) > 64<<10 {
		c.pending = nil // 偶尔的大帧之后不长期占着内存
	}
	return action
}

// LengthField frames messages that start with a fixed-size length header.
type LengthField struct {
	header     int // 1, 2, 4 or 8
	order      binary.ByteOrder
	maxPayload int
}

// NewLengthField returns a framer for frames made of a headerSize-byte (1, 2, 4 or 8)
// unsigned length in the given byte order, followed by that many payload bytes. Frames
// with a payload over maxPayload close the connection with ErrFrameTooLarge.
func NewLengthField(headerSize int, order binary.ByteOrder, maxPayload int) *LengthField {
	switch headerSize {
	case 1, 2, 4, 8:
	default:
		panic(fmt.Sprintf("uringnet: unsupported length header size %d", headerSize))
	}
	return &LengthField{header: headerSize, order: order, maxPayload: maxPayload}
}

// Frame implements Framer.
func (l *LengthField) Frame(buf []byte) (int, error) {
	if len(buf) < l.header {
		return 0, nil
	}
	var size uint64
	switch l.header {
	case 1:
		size = uint64(buf[0])
	case 2:
		size = uint64(l.order.Uint16(buf))
	case 4:
		size = uint64(l.order.Uint32(buf))
	case 8:
		size = l.order.Uint64(buf)
	}
	if size > uint64(l.maxPayload) {
		return 0, fmt.Errorf("%w: %d byte payload, limit %d", ErrFrameTooLarge, size, l.maxPayload)
	}
	n := l.header + int(size)
	if len(buf) < n {
		return 0, nil
	}
	return n, nil
}

// Payload returns the payload of a frame returned by Frame.
func (l *LengthField) Payload(frame []byte) []byte { return frame[l.header:] }

// AppendFrame appends a frame carrying payload to dst.
func (l *LengthField) AppendFrame(dst, payload []byte) []byte {
	if len(payload) > l.maxPayload {
		panic(fmt.Sprintf("uringnet: %d byte payload exceeds the framer limit %d", len(payload), l.maxPayload))
	}
	var h [8]byte
	switch l.header {
	case 1:
		h[0] = byte(len(payload))
	case 2:
		l.order.PutUint16(h[:], uint16(len(payload)))
	case 4:
		l.order.PutUint32(h[:], uint32(len(payload)))
	case 8:
		l.order.PutUint64(h[:], uint64(len(payload)))
	}
	return append(append(dst, h[:l.header]...), payload...)
}

// Delimiter frames messages that end with a delimiter, such as "\n" or "\r\n".
type Delimiter struct {
	delim    []byte
	maxFrame int
}

// NewDelimiter returns a framer for messages terminated by delim. A peer that sends
// maxFrame bytes without a delimiter is closed with ErrFrameTooLarge.
func NewDelimiter(delim []byte, maxFrame int) *Delimiter {
	if len(delim) == 0 {
		panic("uringnet: empty delimiter")
	}
	return &Delimiter{delim: append([]byte(nil), delim...), maxFrame: maxFrame}
}

// Frame implements Framer. The frame includes the delimiter.
func (d *Delimiter) Frame(buf []byte) (int, error) {
	limit := buf
	if len(limit) > d.maxFrame {
		limit = limit[:d.maxFrame]
	}
	if i := bytes.Index(limit, d.delim); i >= 0 {
		return i + len(d.delim), nil
	}
	if len(buf) >= d.maxFrame {
		return 0, fmt.Errorf("%w: no delimiter within %d bytes", ErrFrameTooLarge, d.maxFrame)
	}
	return 0, nil
}

// Payload returns a frame returned by Frame without its delimiter.
func (d *Delimiter) Payload(frame []byte) []byte { return frame[:len(frame)-len(d.delim)] }

// AppendFrame appends payload and the delimiter to dst.
func (d *Delimiter) AppendFrame(dst, payload []byte) []byte {
	return append(append(dst, payload...), d.delim...)
}

// FixedLength frames messages of a fixed size.
type FixedLength int

// Frame implements Framer.
func (f FixedLength) Frame(buf []byte) (int, error) {
	if f <= 0 {
		return 0, fmt.Errorf("uringnet: invalid fixed frame length %d", int(f))
	}
	if len(buf) < int(f) {
		return 0, nil
	}
	return int(f), nil
}
