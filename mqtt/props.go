package mqtt

import (
	"encoding/binary"
	"fmt"
)

// MQTT 5 property identifiers (section 2.2.2.2).
const (
	propPayloadFormat       = 0x01
	propMessageExpiry       = 0x02
	propContentType         = 0x03
	propResponseTopic       = 0x08
	propCorrelationData     = 0x09
	propSubscriptionID      = 0x0B
	propSessionExpiry       = 0x11
	propAssignedClientID    = 0x12
	propServerKeepAlive     = 0x13
	propAuthMethod          = 0x15
	propAuthData            = 0x16
	propRequestProblemInfo  = 0x17
	propWillDelay           = 0x18
	propRequestResponseInfo = 0x19
	propResponseInfo        = 0x1A
	propServerReference     = 0x1C
	propReasonString        = 0x1F
	propReceiveMaximum      = 0x21
	propTopicAliasMaximum   = 0x22
	propTopicAlias          = 0x23
	propMaximumQoS          = 0x24
	propRetainAvailable     = 0x25
	propUserProperty        = 0x26
	propMaximumPacketSize   = 0x27
	propWildcardAvailable   = 0x28
	propSubIDAvailable      = 0x29
	propSharedSubAvailable  = 0x2A
)

// UserProperty is an MQTT 5 user property: a name/value pair carried with a message.
type UserProperty struct {
	Key, Value string
}

// Properties are the MQTT 5 properties of an application message. The broker forwards them
// unchanged to MQTT 5 subscribers; MQTT 3.1.1 subscribers receive the message without them.
type Properties struct {
	PayloadFormat   byte // 1: the payload is UTF-8 text
	ContentType     string
	ResponseTopic   string
	CorrelationData []byte
	User            []UserProperty
}

// props holds the properties decoded from any packet; has records which ones were present.
type props struct {
	has map[byte]bool

	payloadFormat       byte
	messageExpiry       uint32
	contentType         string
	responseTopic       string
	correlationData     []byte
	subscriptionIDs     []int
	sessionExpiry       uint32
	authMethod          string
	requestProblemInfo  byte
	willDelay           uint32
	requestResponseInfo byte
	reasonString        string
	receiveMaximum      uint16
	topicAliasMaximum   uint16
	topicAlias          uint16
	maximumPacketSize   uint32
	user                []UserProperty
}

func (r *reader) varint() int {
	if r.err != nil {
		return 0
	}
	v, n, err := decodeLength(r.b)
	if err != nil || n == 0 {
		r.err = errMalformed
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) uint32() uint32 {
	if r.err != nil || len(r.b) < 4 {
		r.err = errMalformed
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

// parseProps decodes a property list (its length, then the properties). Repeating a property other
// than user properties and subscription identifiers is a protocol error (section 2.2.2.2).
func parseProps(r *reader) (props, error) {
	var p props
	n := r.varint()
	if r.err != nil || n > len(r.b) {
		return p, errMalformed
	}
	pr := &reader{b: r.b[:n]}
	r.b = r.b[n:]
	for pr.err == nil && len(pr.b) > 0 {
		id := byte(pr.varint())
		if p.has == nil {
			p.has = make(map[byte]bool)
		}
		if p.has[id] && id != propUserProperty && id != propSubscriptionID {
			return p, fmt.Errorf("%w: property %#x repeated", errProtocol, id)
		}
		p.has[id] = true
		switch id {
		case propPayloadFormat:
			p.payloadFormat = pr.byte()
		case propMessageExpiry:
			p.messageExpiry = pr.uint32()
		case propContentType:
			p.contentType = pr.string()
		case propResponseTopic:
			p.responseTopic = pr.string()
		case propCorrelationData:
			p.correlationData = append([]byte(nil), pr.bytes()...)
		case propSubscriptionID:
			p.subscriptionIDs = append(p.subscriptionIDs, pr.varint())
		case propSessionExpiry:
			p.sessionExpiry = pr.uint32()
		case propAuthMethod:
			p.authMethod = pr.string()
		case propAuthData:
			pr.bytes()
		case propRequestProblemInfo:
			p.requestProblemInfo = pr.byte()
		case propWillDelay:
			p.willDelay = pr.uint32()
		case propRequestResponseInfo:
			p.requestResponseInfo = pr.byte()
		case propReasonString:
			p.reasonString = pr.string()
		case propReceiveMaximum:
			p.receiveMaximum = pr.uint16()
			if p.receiveMaximum == 0 {
				return p, fmt.Errorf("%w: receive maximum 0", errProtocol)
			}
		case propTopicAliasMaximum:
			p.topicAliasMaximum = pr.uint16()
		case propTopicAlias:
			p.topicAlias = pr.uint16()
		case propMaximumPacketSize:
			p.maximumPacketSize = pr.uint32()
			if p.maximumPacketSize == 0 {
				return p, fmt.Errorf("%w: maximum packet size 0", errProtocol)
			}
		case propUserProperty:
			k := pr.string()
			v := pr.string()
			p.user = append(p.user, UserProperty{k, v})
		case propServerKeepAlive:
			pr.uint16()
		case propAssignedClientID, propResponseInfo, propServerReference:
			pr.string()
		case propMaximumQoS, propRetainAvailable, propWildcardAvailable, propSubIDAvailable, propSharedSubAvailable:
			pr.byte()
		default:
			return p, fmt.Errorf("%w: unknown property %#x", errMalformed, id)
		}
	}
	if pr.err != nil {
		return p, pr.err
	}
	return p, nil
}

// messageProps extracts the forwardable message properties, nil if there are none.
func (p *props) messageProps() *Properties {
	if !p.has[propPayloadFormat] && !p.has[propContentType] && !p.has[propResponseTopic] &&
		!p.has[propCorrelationData] && len(p.user) == 0 {
		return nil
	}
	return &Properties{PayloadFormat: p.payloadFormat, ContentType: p.contentType, ResponseTopic: p.responseTopic,
		CorrelationData: p.correlationData, User: p.user}
}

// propWriter builds a property list.
type propWriter struct{ b []byte }

func (w *propWriter) byte(id, v byte) { w.b = append(w.b, id, v) }
func (w *propWriter) uint16(id byte, v uint16) {
	w.b = binary.BigEndian.AppendUint16(append(w.b, id), v)
}
func (w *propWriter) uint32(id byte, v uint32) {
	w.b = binary.BigEndian.AppendUint32(append(w.b, id), v)
}
func (w *propWriter) string(id byte, s string) { w.b = appendString(append(w.b, id), s) }
func (w *propWriter) bytes(id byte, b []byte)  { w.b = appendBytes(append(w.b, id), b) }

func (w *propWriter) message(m *Properties, expirySecs uint32) {
	if expirySecs > 0 {
		w.uint32(propMessageExpiry, expirySecs)
	}
	if m == nil {
		return
	}
	if m.PayloadFormat != 0 {
		w.byte(propPayloadFormat, m.PayloadFormat)
	}
	if m.ContentType != "" {
		w.string(propContentType, m.ContentType)
	}
	if m.ResponseTopic != "" {
		w.string(propResponseTopic, m.ResponseTopic)
	}
	if m.CorrelationData != nil {
		w.bytes(propCorrelationData, m.CorrelationData)
	}
	for _, u := range m.User {
		w.b = append(w.b, propUserProperty)
		w.b = appendString(w.b, u.Key)
		w.b = appendString(w.b, u.Value)
	}
}

// appendTo appends the property list (length, then properties) to dst.
func (w *propWriter) appendTo(dst []byte) []byte {
	return append(appendLength(dst, len(w.b)), w.b...)
}
