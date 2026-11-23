// Package mqtt is an MQTT broker (versions 3.1.1 and 5) on uringnet, for IoT gateways: devices
// connect and publish, and the application in the same process receives every message (OnPublish)
// and can publish to devices (Publish).
package mqtt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
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
	// Expiry is the MQTT 5 message expiry interval: undelivered copies are discarded after it,
	// and MQTT 5 subscribers receive the remaining time. 0 means no expiry.
	Expiry time.Duration
	// Properties are MQTT 5 properties, forwarded to MQTT 5 subscribers; nil if none.
	Properties *Properties
}

// Protocol versions (the CONNECT protocol level).
const (
	version31  = 3
	version311 = 4
	version5   = 5
)

// Packet types (section 2.1.2).
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
	typeAuth        = 15
)

// MQTT 3.1.1 CONNACK return codes.
const (
	connAccepted          = 0
	connBadProtocol       = 1
	connIdentifierInvalid = 2
	connBadCredentials    = 4
	connNotAuthorized     = 5
)

// MQTT 5 reason codes (section 2.4) used by the broker.
const (
	rcSuccess               = 0x00
	rcGrantedQoS1           = 0x01
	rcDisconnectWithWill    = 0x04
	rcNoSubscriptionExisted = 0x11
	rcUnspecified           = 0x80
	rcMalformed             = 0x81
	rcProtocolError         = 0x82
	rcUnsupportedVersion    = 0x84
	rcClientIDInvalid       = 0x85
	rcBadCredentials        = 0x86
	rcNotAuthorized         = 0x87
	rcBadAuthMethod         = 0x8C
	rcKeepAliveTimeout      = 0x8D
	rcSessionTakenOver      = 0x8E
	rcTopicFilterInvalid    = 0x8F
	rcTopicNameInvalid      = 0x90
	rcPacketIDNotFound      = 0x92
	rcReceiveMaxExceeded    = 0x93
	rcTopicAliasInvalid     = 0x94
	rcPacketTooLarge        = 0x95
	rcSharedSubNotSupported = 0x9E
	rcSubIDsNotSupported    = 0xA1
)

var (
	errMalformed      = errors.New("mqtt: malformed packet")
	errProtocol       = errors.New("mqtt: protocol violation")
	errPacketTooLarge = errors.New("mqtt: packet too large")
	errBadProtocol    = errors.New("mqtt: unsupported protocol version")
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
	return 0, 0, fmt.Errorf("%w: variable byte integer longer than 4 bytes", errMalformed)
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

// varintSize is the length of n encoded as a variable byte integer.
func varintSize(n int) int {
	switch {
	case n < 128:
		return 1
	case n < 16384:
		return 2
	case n < 2097152:
		return 3
	}
	return 4
}

// header splits a complete packet into type, flags and body.
func header(pkt []byte) (typ, flags byte, body []byte) {
	_, n, _ := decodeLength(pkt[1:])
	return pkt[0] >> 4, pkt[0] & 0x0f, pkt[1+n:]
}

// packet prefixes body with the fixed header.
func packet(dst []byte, first byte, body []byte) []byte {
	return append(appendLength(append(dst, first), len(body)), body...)
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

// string reads a UTF-8 string, which must not contain U+0000 (section 1.5.4).
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
	version     byte
	cleanStart  bool // "clean session" in 3.1.1
	keepAlive   uint16
	clientID    string
	will        *Message
	willDelay   uint32
	username    string
	hasUsername bool
	password    []byte
	hasPassword bool
	props       props // MQTT 5
}

func parseConnect(flags byte, body []byte) (*connectPacket, error) {
	if flags != 0 {
		return nil, errMalformed
	}
	r := &reader{b: body}
	name := r.string()
	p := &connectPacket{version: r.byte()}
	cf := r.byte()
	p.keepAlive = r.uint16()
	if r.err != nil {
		return nil, r.err
	}
	switch {
	case name == "MQTT" && (p.version == version311 || p.version == version5):
	case name == "MQIsdp" && p.version == version31:
	default:
		return p, errBadProtocol
	}
	if cf&0x01 != 0 { // reserved flag must be 0
		return nil, errMalformed
	}
	p.cleanStart = cf&0x02 != 0
	if p.version == version5 {
		pp, err := parseProps(r)
		if err != nil {
			return nil, err
		}
		p.props = pp
	}
	p.clientID = r.string()
	if cf&0x04 != 0 { // will flag
		w := &Message{QoS: (cf >> 3) & 0x03, Retain: cf&0x20 != 0}
		if w.QoS > 2 {
			return nil, errMalformed
		}
		if p.version == version5 {
			wp, err := parseProps(r)
			if err != nil {
				return nil, err
			}
			w.Expiry = time.Duration(wp.messageExpiry) * time.Second
			w.Properties = wp.messageProps()
			p.willDelay = wp.willDelay
		}
		w.Topic = r.string()
		w.Payload = append([]byte(nil), r.bytes()...)
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

// appendConnack builds a CONNACK. For MQTT 5, code is a reason code and w holds the properties.
func appendConnack(dst []byte, version byte, sessionPresent bool, code byte, w *propWriter) []byte {
	sp := byte(0)
	if sessionPresent {
		sp = 1
	}
	if version != version5 {
		return append(dst, typeConnack<<4, 2, sp, code)
	}
	body := []byte{sp, code}
	if w == nil {
		w = &propWriter{}
	}
	return packet(dst, typeConnack<<4, w.appendTo(body))
}

// publishPacket is a decoded PUBLISH; payload aliases the received buffer.
type publishPacket struct {
	dup, retain bool
	qos         byte
	id          uint16
	topic       string
	payload     []byte
	props       *props // MQTT 5 only, nil otherwise
}

func parsePublish(flags byte, body []byte, version byte) (*publishPacket, error) {
	p := &publishPacket{dup: flags&0x08 != 0, qos: (flags >> 1) & 0x03, retain: flags&0x01 != 0}
	if p.qos > 2 || (p.qos == 0 && p.dup) {
		return nil, errMalformed
	}
	r := &reader{b: body}
	p.topic = r.string()
	if p.qos > 0 {
		p.id = r.uint16()
		if r.err == nil && p.id == 0 {
			return nil, errMalformed
		}
	}
	if version == version5 {
		pp, err := parseProps(r)
		if err != nil {
			return nil, err
		}
		p.props = &pp
	}
	if r.err != nil {
		return nil, r.err
	}
	p.payload = r.b
	return p, nil
}

// appendPublish builds a PUBLISH. For MQTT 5 it carries mp and the remaining expiry (seconds, 0 for none).
func appendPublish(dst []byte, version byte, topic string, payload []byte, qos byte, retain, dup bool, id uint16, mp *Properties, expirySecs uint32) []byte {
	b := byte(typePublish<<4) | qos<<1
	if retain {
		b |= 0x01
	}
	if dup {
		b |= 0x08
	}
	var pw propWriter
	if version == version5 {
		pw.message(mp, expirySecs)
	}
	n := 2 + len(topic) + len(payload)
	if qos > 0 {
		n += 2
	}
	if version == version5 {
		n += varintSize(len(pw.b)) + len(pw.b)
	}
	if dst == nil {
		dst = make([]byte, 0, 1+varintSize(n)+n)
	}
	dst = append(dst, b)
	dst = appendLength(dst, n)
	dst = appendString(dst, topic)
	if qos > 0 {
		dst = binary.BigEndian.AppendUint16(dst, id)
	}
	if version == version5 {
		dst = pw.appendTo(dst)
	}
	return append(dst, payload...)
}

// appendAck builds PUBACK, PUBREC, PUBREL or PUBCOMP. MQTT 5 adds the reason code unless it is 0.
func appendAck(dst []byte, version, typ byte, id uint16, reason byte) []byte {
	flags := byte(0)
	if typ == typePubrel {
		flags = 0x02
	}
	if version == version5 && reason != rcSuccess {
		return append(binary.BigEndian.AppendUint16(append(dst, typ<<4|flags, 3), id), reason)
	}
	return binary.BigEndian.AppendUint16(append(dst, typ<<4|flags, 2), id)
}

// parseAck decodes PUBACK, PUBREC, PUBREL and PUBCOMP (MQTT 5 may add a reason code and properties).
func parseAck(body []byte, version byte) (id uint16, reason byte, err error) {
	if len(body) < 2 || (version != version5 && len(body) != 2) {
		return 0, 0, errMalformed
	}
	id = binary.BigEndian.Uint16(body)
	if len(body) > 2 {
		reason = body[2]
	}
	return id, reason, nil
}

// subscription is one filter of a SUBSCRIBE with its options.
type subscription struct {
	filter            string
	qos               byte
	noLocal           bool // MQTT 5: do not send the subscriber's own messages back
	retainAsPublished bool // MQTT 5: keep the retain flag when forwarding
	retainHandling    byte // MQTT 5: 0 send retained, 1 only for a new subscription, 2 never
}

func parseSubscribe(flags byte, body []byte, version byte) (uint16, []subscription, props, error) {
	var pp props
	if flags != 0x02 {
		return 0, nil, pp, errMalformed
	}
	r := &reader{b: body}
	id := r.uint16()
	if version == version5 {
		var err error
		if pp, err = parseProps(r); err != nil {
			return 0, nil, pp, err
		}
	}
	var subs []subscription
	for r.err == nil && len(r.b) > 0 {
		f := r.string()
		o := r.byte()
		s := subscription{filter: f, qos: o & 0x03}
		if version == version5 {
			s.noLocal = o&0x04 != 0
			s.retainAsPublished = o&0x08 != 0
			s.retainHandling = (o >> 4) & 0x03
			if o&0xC0 != 0 || s.retainHandling > 2 {
				return 0, nil, pp, errMalformed
			}
		} else if o&0xFC != 0 {
			return 0, nil, pp, errMalformed
		}
		if s.qos > 2 {
			return 0, nil, pp, errMalformed
		}
		subs = append(subs, s)
	}
	if r.err != nil || len(subs) == 0 || id == 0 {
		return 0, nil, pp, errMalformed
	}
	return id, subs, pp, nil
}

func appendSuback(dst []byte, version byte, id uint16, codes []byte) []byte {
	body := binary.BigEndian.AppendUint16(nil, id)
	if version == version5 {
		body = append(body, 0) // no properties
	}
	return packet(dst, typeSuback<<4, append(body, codes...))
}

func parseUnsubscribe(flags byte, body []byte, version byte) (uint16, []string, error) {
	if flags != 0x02 {
		return 0, nil, errMalformed
	}
	r := &reader{b: body}
	id := r.uint16()
	if version == version5 {
		if _, err := parseProps(r); err != nil {
			return 0, nil, err
		}
	}
	var filters []string
	for r.err == nil && len(r.b) > 0 {
		filters = append(filters, r.string())
	}
	if r.err != nil || len(filters) == 0 || id == 0 {
		return 0, nil, errMalformed
	}
	return id, filters, nil
}

// appendUnsuback builds an UNSUBACK; MQTT 5 has a reason code per filter.
func appendUnsuback(dst []byte, version byte, id uint16, codes []byte) []byte {
	body := binary.BigEndian.AppendUint16(nil, id)
	if version == version5 {
		body = append(append(body, 0), codes...)
	}
	return packet(dst, typeUnsuback<<4, body)
}

// parseDisconnect decodes a DISCONNECT. MQTT 5 may carry a reason code and properties.
func parseDisconnect(flags byte, body []byte, version byte) (byte, props, error) {
	var pp props
	if flags != 0 {
		return 0, pp, errMalformed
	}
	if version != version5 {
		if len(body) != 0 {
			return 0, pp, errMalformed
		}
		return rcSuccess, pp, nil
	}
	if len(body) == 0 {
		return rcSuccess, pp, nil
	}
	r := &reader{b: body}
	reason := r.byte()
	if len(r.b) > 0 {
		var err error
		if pp, err = parseProps(r); err != nil {
			return 0, pp, err
		}
	}
	return reason, pp, r.err
}

// appendDisconnect builds an MQTT 5 DISCONNECT with a reason code.
func appendDisconnect(dst []byte, reason byte) []byte {
	if reason == rcSuccess {
		return append(dst, typeDisconnect<<4, 0)
	}
	return append(dst, typeDisconnect<<4, 1, reason)
}
