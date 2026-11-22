// Package mqtt is an MQTT 3.1.1 broker on uringnet, for IoT gateways: devices connect and publish,
// and the application in the same process receives every message (OnPublish) and can publish to
// devices (Publish).
package mqtt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Message is an application message.
type Message struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
	// ClientID is the publishing client, "" for messages from Broker.Publish.
	ClientID string
}

// Packet types (MQTT 3.1.1 section 2.2.1).
const (
	typeConnect     = 1
	typeConnack     = 2
	typePublish     = 3
	typePuback      = 4
	typePubrec      = 5
	typePubrel      = 6
	typePubcomp     = 7
	typeSubscribe   = 8
	typeSuback      = 9
	typeUnsubscribe = 10
	typeUnsuback    = 11
	typePingreq     = 12
	typePingresp    = 13
	typeDisconnect  = 14
)

// CONNACK return codes.
const (
	connAccepted          = 0
	connBadProtocol       = 1
	connIdentifierInvalid = 2
	connBadCredentials    = 4
	connNotAuthorized     = 5
)

var (
	errMalformed      = errors.New("mqtt: malformed packet")
	errPacketTooLarge = errors.New("mqtt: packet too large")
)

// framer cuts MQTT packets from a byte stream: one byte of type and flags, then the remaining
// length as a variable byte integer (1 to 4 bytes), then that many bytes.
type framer struct{ max int }

func (f framer) Frame(buf []byte) (int, error) {
	if len(buf) < 2 {
		return 0, nil
	}
	length, n, err := decodeLength(buf[1:])
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil // length incomplete
	}
	total := 1 + n + length
	if total > f.max {
		return 0, fmt.Errorf("%w: %d bytes, limit %d", errPacketTooLarge, total, f.max)
	}
	if len(buf) < total {
		return 0, nil
	}
	return total, nil
}

// decodeLength decodes a variable byte integer; n is 0 if more bytes are needed.
func decodeLength(b []byte) (length, n int, err error) {
	mult := 1
	for i := 0; i < 4; i++ {
		if i >= len(b) {
			return 0, 0, nil
		}
		length += int(b[i]&0x7f) * mult
		if b[i]&0x80 == 0 {
			return length, i + 1, nil
		}
		mult *= 128
	}
	return 0, 0, fmt.Errorf("%w: remaining length longer than 4 bytes", errMalformed)
}

func appendLength(dst []byte, n int) []byte {
	for {
		b := byte(n % 128)
		n /= 128
		if n > 0 {
			b |= 0x80
		}
		dst = append(dst, b)
		if n == 0 {
			return dst
		}
	}
}

// header splits a complete packet into type, flags and body.
func header(pkt []byte) (typ, flags byte, body []byte) {
	_, n, _ := decodeLength(pkt[1:])
	return pkt[0] >> 4, pkt[0] & 0x0f, pkt[1+n:]
}

// reader decodes the fields of a packet body.
type reader struct {
	b   []byte
	err error
}

func (r *reader) byte() byte {
	if r.err != nil || len(r.b) < 1 {
		r.err = errMalformed
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *reader) uint16() uint16 {
	if r.err != nil || len(r.b) < 2 {
		r.err = errMalformed
		return 0
	}
	v := binary.BigEndian.Uint16(r.b)
	r.b = r.b[2:]
	return v
}

func (r *reader) bytes() []byte {
	n := int(r.uint16())
	if r.err != nil || len(r.b) < n {
		r.err = errMalformed
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

// string reads a UTF-8 string, which must not contain U+0000 (section 1.5.3).
func (r *reader) string() string {
	b := r.bytes()
	if r.err == nil && (!utf8.Valid(b) || containsNUL(b)) {
		r.err = fmt.Errorf("%w: invalid UTF-8 string", errMalformed)
	}
	return string(b)
}

func containsNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

func appendString(dst []byte, s string) []byte {
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(s)))
	return append(dst, s...)
}

func appendBytes(dst, b []byte) []byte {
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(b)))
	return append(dst, b...)
}

// connectPacket is a decoded CONNECT.
type connectPacket struct {
	protocolLevel byte
	cleanSession  bool
	keepAlive     uint16
	clientID      string
	will          *Message
	username      string
	hasUsername   bool
	password      []byte
	hasPassword   bool
}

func parseConnect(flags byte, body []byte) (*connectPacket, error) {
	if flags != 0 {
		return nil, errMalformed
	}
	r := &reader{b: body}
	name := r.string()
	p := &connectPacket{protocolLevel: r.byte()}
	cf := r.byte()
	p.keepAlive = r.uint16()
	if r.err != nil {
		return nil, r.err
	}
	if !(name == "MQTT" && p.protocolLevel == 4) && !(name == "MQIsdp" && p.protocolLevel == 3) {
		return p, errBadProtocol
	}
	if cf&0x01 != 0 { // reserved flag must be 0
		return nil, errMalformed
	}
	p.cleanSession = cf&0x02 != 0
	p.clientID = r.string()
	if cf&0x04 != 0 { // will flag
		w := &Message{QoS: (cf >> 3) & 0x03, Retain: cf&0x20 != 0}
		w.Topic = r.string()
		w.Payload = append([]byte(nil), r.bytes()...)
		if w.QoS > 2 {
			return nil, errMalformed
		}
		p.will = w
	} else if cf&0x38 != 0 { // will QoS and retain must be 0 without a will
		return nil, errMalformed
	}
	if cf&0x80 != 0 {
		p.username, p.hasUsername = r.string(), true
	}
	if cf&0x40 != 0 {
		p.password, p.hasPassword = append([]byte(nil), r.bytes()...), true
	}
	if r.err != nil {
		return nil, r.err
	}
	return p, nil
}

var errBadProtocol = errors.New("mqtt: unsupported protocol version")

func appendConnack(dst []byte, sessionPresent bool, code byte) []byte {
	sp := byte(0)
	if sessionPresent {
		sp = 1
	}
	return append(dst, typeConnack<<4, 2, sp, code)
}

// publishPacket is a decoded PUBLISH; topic and payload alias the received buffer.
type publishPacket struct {
	dup, retain bool
	qos         byte
	id          uint16
	topic       string
	payload     []byte
}

func parsePublish(flags byte, body []byte) (*publishPacket, error) {
	p := &publishPacket{dup: flags&0x08 != 0, qos: (flags >> 1) & 0x03, retain: flags&0x01 != 0}
	if p.qos > 2 || (p.qos == 0 && p.dup) {
		return nil, errMalformed
	}
	r := &reader{b: body}
	p.topic = r.string()
	if p.qos > 0 {
		p.id = r.uint16()
		if p.id == 0 {
			return nil, errMalformed
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	p.payload = r.b
	return p, nil
}

func appendPublish(dst []byte, topic string, payload []byte, qos byte, retain, dup bool, id uint16) []byte {
	b := byte(typePublish<<4) | qos<<1
	if retain {
		b |= 0x01
	}
	if dup {
		b |= 0x08
	}
	n := 2 + len(topic) + len(payload)
	if qos > 0 {
		n += 2
	}
	dst = append(dst, b)
	dst = appendLength(dst, n)
	dst = appendString(dst, topic)
	if qos > 0 {
		dst = binary.BigEndian.AppendUint16(dst, id)
	}
	return append(dst, payload...)
}

// appendAck builds PUBACK, PUBREC, PUBREL, PUBCOMP and UNSUBACK: type, flags, length 2, packet id.
func appendAck(dst []byte, typ byte, id uint16) []byte {
	flags := byte(0)
	if typ == typePubrel {
		flags = 0x02
	}
	return binary.BigEndian.AppendUint16(append(dst, typ<<4|flags, 2), id)
}

func parseID(body []byte) (uint16, error) {
	if len(body) != 2 {
		return 0, errMalformed
	}
	return binary.BigEndian.Uint16(body), nil
}

type subscription struct {
	filter string
	qos    byte
}

func parseSubscribe(flags byte, body []byte) (uint16, []subscription, error) {
	if flags != 0x02 {
		return 0, nil, errMalformed
	}
	r := &reader{b: body}
	id := r.uint16()
	var subs []subscription
	for r.err == nil && len(r.b) > 0 {
		f := r.string()
		q := r.byte()
		if q > 2 {
			return 0, nil, errMalformed
		}
		subs = append(subs, subscription{f, q})
	}
	if r.err != nil || len(subs) == 0 || id == 0 {
		return 0, nil, errMalformed
	}
	return id, subs, nil
}

func appendSuback(dst []byte, id uint16, codes []byte) []byte {
	dst = append(dst, typeSuback<<4)
	dst = appendLength(dst, 2+len(codes))
	dst = binary.BigEndian.AppendUint16(dst, id)
	return append(dst, codes...)
}

func parseUnsubscribe(flags byte, body []byte) (uint16, []string, error) {
	if flags != 0x02 {
		return 0, nil, errMalformed
	}
	r := &reader{b: body}
	id := r.uint16()
	var filters []string
	for r.err == nil && len(r.b) > 0 {
		filters = append(filters, r.string())
	}
	if r.err != nil || len(filters) == 0 || id == 0 {
		return 0, nil, errMalformed
	}
	return id, filters, nil
}
