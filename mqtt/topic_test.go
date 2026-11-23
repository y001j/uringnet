package mqtt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func TestValidFilter(t *testing.T) {
	for f, want := range map[string]bool{
		"a/b": true, "+": true, "#": true, "a/+/c": true, "a/#": true, "+/+": true, "/": true,
		"": false, "a/#/c": false, "a+": false, "a/b#": false, "#/a": false, "a/+b": false,
	} {
		if got := validFilter(f); got != want {
			t.Errorf("validFilter(%q) = %v", f, got)
		}
	}
}

// Examples from MQTT 3.1.1 section 4.7.
func TestMatchFilter(t *testing.T) {
	for _, tc := range []struct {
		filter, topic string
		want          bool
	}{
		{"sport/tennis/player1/#", "sport/tennis/player1", true},
		{"sport/tennis/player1/#", "sport/tennis/player1/ranking", true},
		{"sport/tennis/player1/#", "sport/tennis/player1/score/wimbledon", true},
		{"sport/#", "sport", true},
		{"#", "anything/at/all", true},
		{"sport/tennis/+", "sport/tennis/player1", true},
		{"sport/tennis/+", "sport/tennis/player1/ranking", false},
		{"sport/+", "sport", false},
		{"sport/+", "sport/", true},
		{"+/+", "/finance", true},
		{"/+", "/finance", true},
		{"+", "/finance", false},
		{"#", "$SYS/broker", false},
		{"+/monitor/Clients", "$SYS/monitor/Clients", false},
		{"$SYS/#", "$SYS/broker", true},
		{"$SYS/monitor/+", "$SYS/monitor/Clients", true},
		{"a/b", "a/b", true},
		{"a/b", "a/c", false},
	} {
		if got := matchFilter(tc.filter, tc.topic); got != tc.want {
			t.Errorf("matchFilter(%q, %q) = %v", tc.filter, tc.topic, got)
		}
	}
}

// The trie must agree with matching every filter one by one.
func TestSubscriptionsMatchAgreesWithFilters(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	words := []string{"a", "b", "c", "$SYS", ""}
	randTopic := func() string {
		n := 1 + rng.Intn(4)
		l := make([]string, n)
		for i := range l {
			l[i] = words[rng.Intn(len(words))]
		}
		return strings.Join(l, "/")
	}
	randFilter := func() string {
		n := 1 + rng.Intn(4)
		l := make([]string, n)
		for i := range l {
			switch rng.Intn(6) {
			case 0:
				l[i] = "+"
			case 1:
				if i == n-1 {
					l[i] = "#"
					continue
				}
				fallthrough
			default:
				l[i] = words[rng.Intn(len(words))]
			}
		}
		return strings.Join(l, "/")
	}
	subs := newSubscriptions[int, byte]()
	filters := map[int][]string{}
	for id := 0; id < 50; id++ {
		for k := 0; k < 3; k++ {
			f := randFilter()
			if validFilter(f) {
				subs.add(f, id, byte(rng.Intn(2)))
				filters[id] = append(filters[id], f)
			}
		}
	}
	for i := 0; i < 2000; i++ {
		topic := randTopic()
		var got, want []int
		subs.match(topic, func(a, b byte) byte { return max(a, b) }, func(id int, _ byte) { got = append(got, id) })
		for id, fs := range filters {
			for _, f := range fs {
				if matchFilter(f, topic) {
					want = append(want, id)
					break
				}
			}
		}
		sort.Ints(got)
		sort.Ints(want)
		if len(got) != len(want) || (len(got) > 0 && !equalInts(got, want)) {
			t.Fatalf("topic %q: trie matched %v, filters match %v", topic, got, want)
		}
	}
	// Removing every subscription leaves an empty trie
	for id, fs := range filters {
		for _, f := range fs {
			subs.remove(f, id)
		}
	}
	if subs.count() != 0 || len(subs.root.children) != 0 {
		t.Fatalf("after removing everything: count %d, %d root children", subs.count(), len(subs.root.children))
	}
}

func equalInts(a, b []int) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFramerAndLengths(t *testing.T) {
	for _, n := range []int{0, 1, 127, 128, 16383, 16384, 2097151, 2097152, 268435455} {
		enc := appendLength(nil, n)
		got, k, err := decodeLength(enc)
		if err != nil || got != n || k != len(enc) {
			t.Fatalf("length %d: decoded %d, %d bytes, %v", n, got, k, err)
		}
	}
	f := framer{max: 1 << 20}
	pkt := appendPublish(nil, version311, "a/b", bytes.Repeat([]byte("x"), 300), 1, false, false, 7, nil, 0)
	for i := 0; i < len(pkt); i++ {
		if n, err := f.Frame(pkt[:i]); n != 0 || err != nil {
			t.Fatalf("partial packet of %d bytes: %d, %v", i, n, err)
		}
	}
	if n, err := f.Frame(append(pkt, 0xc0, 0)); n != len(pkt) || err != nil {
		t.Fatalf("Frame = %d, %v, want %d", n, err, len(pkt))
	}
	if _, err := f.Frame([]byte{0x30, 0xff, 0xff, 0xff, 0xff, 0x01}); err == nil {
		t.Fatal("5-byte remaining length accepted")
	}
	if _, err := (framer{max: 100}).Frame(pkt); !errors.Is(err, errPacketTooLarge) {
		t.Fatalf("oversized packet: %v", err)
	}
}

func TestPublishRoundTrip(t *testing.T) {
	pkt := appendPublish(nil, version311, "sensors/1", []byte("21.5"), 1, true, true, 42, nil, 0)
	typ, flags, body := header(pkt)
	if typ != typePublish {
		t.Fatal(typ)
	}
	p, err := parsePublish(flags, body, version311)
	if err != nil || p.topic != "sensors/1" || string(p.payload) != "21.5" || p.qos != 1 || !p.retain || !p.dup || p.id != 42 {
		t.Fatalf("%+v %v", p, err)
	}
	// QoS 0 with DUP, QoS 3, packet id 0 and invalid UTF-8 are rejected
	for _, bad := range [][]byte{
		appendPublish(nil, version311, "t", nil, 0, false, true, 0, nil, 0),
		{typePublish<<4 | 0x06, 3, 0, 1, 't'},
		appendPublish(nil, version311, "t", nil, 1, false, false, 0, nil, 0),
		{typePublish << 4, 4, 0, 2, 0xff, 0xfe},
	} {
		_, flags, body := header(bad)
		if _, err := parsePublish(flags, body, version311); err == nil {
			t.Errorf("accepted malformed PUBLISH % x", bad)
		}
	}
}

func TestParseConnect(t *testing.T) {
	body := appendString(nil, "MQTT")
	body = append(body, 4, 0xC2|0x04|0x08, 0, 60) // username, password, will QoS 1, clean session
	body = appendString(body, "dev-1")
	body = appendString(body, "dev/1/status")
	body = appendBytes(body, []byte("offline"))
	body = appendString(body, "user")
	body = appendBytes(body, []byte("secret"))
	p, err := parseConnect(0, body)
	if err != nil {
		t.Fatal(err)
	}
	if p.clientID != "dev-1" || !p.cleanStart || p.keepAlive != 60 || p.username != "user" || string(p.password) != "secret" ||
		p.will == nil || p.will.Topic != "dev/1/status" || string(p.will.Payload) != "offline" || p.will.QoS != 1 {
		t.Fatalf("%+v", p)
	}
	v6 := appendString(nil, "MQTT")
	v6 = append(v6, 6, 0x02, 0, 0)
	if _, err := parseConnect(0, v6); !errors.Is(err, errBadProtocol) {
		t.Fatalf("MQTT 6 CONNECT: %v", err)
	}
	reserved := appendString(nil, "MQTT")
	reserved = append(reserved, 4, 0x03, 0, 0) // reserved flag set
	reserved = appendString(reserved, "x")
	if _, err := parseConnect(0, reserved); err == nil {
		t.Fatal("reserved connect flag accepted")
	}
}

func TestPropertiesRoundTrip(t *testing.T) {
	mp := &Properties{PayloadFormat: 1, ContentType: "application/json", ResponseTopic: "reply/1",
		CorrelationData: []byte{1, 2, 3}, User: []UserProperty{{"a", "1"}, {"a", "2"}, {"model", "x9"}}}
	pkt := appendPublish(nil, version5, "t/1", []byte("{}"), 1, false, false, 9, mp, 30)
	_, flags, body := header(pkt)
	if f, err := (framer{max: 1 << 20}).Frame(pkt); f != len(pkt) || err != nil {
		t.Fatalf("Frame = %d, %v, want %d", f, err, len(pkt))
	}
	p, err := parsePublish(flags, body, version5)
	if err != nil || p.topic != "t/1" || string(p.payload) != "{}" || p.id != 9 || p.props.messageExpiry != 30 {
		t.Fatalf("%+v %v", p, err)
	}
	got := p.props.messageProps()
	if got == nil || got.PayloadFormat != 1 || got.ContentType != mp.ContentType || got.ResponseTopic != mp.ResponseTopic ||
		!bytes.Equal(got.CorrelationData, mp.CorrelationData) || len(got.User) != 3 || got.User[1] != (UserProperty{"a", "2"}) {
		t.Fatalf("properties %+v", got)
	}
	// Without properties: an empty property list, and nil message properties
	pkt = appendPublish(nil, version5, "t", []byte("x"), 0, false, false, 0, nil, 0)
	_, flags, body = header(pkt)
	if p, err := parsePublish(flags, body, version5); err != nil || p.props.messageProps() != nil || string(p.payload) != "x" {
		t.Fatalf("%+v %v", p, err)
	}
	// Repeated properties (other than user properties), receive maximum 0, unknown ids and
	// truncated lists are errors
	for _, raw := range [][]byte{
		{4, propContentType, 0, 0, propContentType},
		{6, propContentType, 0, 0, propContentType, 0, 0},
		{3, propReceiveMaximum, 0, 0},
		{2, 0x7f, 0},
		{5, propMessageExpiry, 0, 0},
		{9},
	} {
		if _, err := parseProps(&reader{b: raw}); err == nil {
			t.Errorf("accepted properties % x", raw)
		}
	}
}

func TestParseConnectV5(t *testing.T) {
	var cp, wp propWriter
	cp.uint32(propSessionExpiry, 3600)
	cp.uint16(propReceiveMaximum, 10)
	cp.uint32(propMaximumPacketSize, 4096)
	wp.uint32(propWillDelay, 5)
	wp.uint32(propMessageExpiry, 60)
	wp.string(propContentType, "text/plain")
	body := appendString(nil, "MQTT")
	body = append(body, 5, 0x04|0x08, 0, 30) // will QoS 1, clean start 0
	body = cp.appendTo(body)
	body = appendString(body, "dev-5")
	body = wp.appendTo(body)
	body = appendString(body, "dev/5/status")
	body = appendBytes(body, []byte("offline"))
	p, err := parseConnect(0, body)
	if err != nil {
		t.Fatal(err)
	}
	if p.version != 5 || p.cleanStart || p.clientID != "dev-5" || p.props.sessionExpiry != 3600 || p.props.receiveMaximum != 10 ||
		p.props.maximumPacketSize != 4096 || p.willDelay != 5 || p.will == nil || p.will.Expiry.Seconds() != 60 ||
		p.will.Properties == nil || p.will.Properties.ContentType != "text/plain" || p.will.Topic != "dev/5/status" {
		t.Fatalf("%+v", p)
	}
}

func TestV5Acks(t *testing.T) {
	// A reason code is sent only when it is not success; both forms parse
	for _, reason := range []byte{rcSuccess, rcNotAuthorized} {
		pkt := appendAck(nil, version5, typePuback, 3, reason)
		_, _, body := header(pkt)
		id, r, err := parseAck(body, version5)
		if err != nil || id != 3 || r != reason {
			t.Fatalf("reason %#x: % x -> %d %#x %v", reason, pkt, id, r, err)
		}
	}
	if _, _, err := parseAck([]byte{0, 1, 0}, version311); err == nil {
		t.Fatal("MQTT 3.1.1 ack with a reason code accepted")
	}
	body := binary.BigEndian.AppendUint16(nil, 1)
	body = append(body, 0) // no properties
	body = appendString(body, "a/+")
	body = append(body, 0x01|0x04|0x08|0x20) // QoS 1, no local, retain as published, retain handling 2
	id, subs, _, err := parseSubscribe(0x02, body, version5)
	if err != nil || id != 1 || len(subs) != 1 || subs[0].qos != 1 || !subs[0].noLocal || !subs[0].retainAsPublished || subs[0].retainHandling != 2 {
		t.Fatalf("%+v %v", subs, err)
	}
	body[len(body)-1] = 0x30 // retain handling 3 is malformed
	if _, _, _, err := parseSubscribe(0x02, body, version5); err == nil {
		t.Fatal("retain handling 3 accepted")
	}
	if _, _, _, err := parseSubscribe(0x02, body, version311); err == nil {
		t.Fatal("MQTT 5 subscription options accepted from an MQTT 3.1.1 client")
	}
}
