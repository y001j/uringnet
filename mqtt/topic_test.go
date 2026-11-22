package mqtt

import (
	"bytes"
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
	subs := newSubscriptions[int]()
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
		subs.match(topic, func(id int, _ byte) { got = append(got, id) })
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
	pkt := appendPublish(nil, "a/b", bytes.Repeat([]byte("x"), 300), 1, false, false, 7)
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
	pkt := appendPublish(nil, "sensors/1", []byte("21.5"), 1, true, true, 42)
	typ, flags, body := header(pkt)
	if typ != typePublish {
		t.Fatal(typ)
	}
	p, err := parsePublish(flags, body)
	if err != nil || p.topic != "sensors/1" || string(p.payload) != "21.5" || p.qos != 1 || !p.retain || !p.dup || p.id != 42 {
		t.Fatalf("%+v %v", p, err)
	}
	// QoS 0 with DUP, QoS 3, packet id 0 and invalid UTF-8 are rejected
	for _, bad := range [][]byte{
		appendPublish(nil, "t", nil, 0, false, true, 0),
		{typePublish<<4 | 0x06, 3, 0, 1, 't'},
		appendPublish(nil, "t", nil, 1, false, false, 0),
		{typePublish << 4, 4, 0, 2, 0xff, 0xfe},
	} {
		_, flags, body := header(bad)
		if _, err := parsePublish(flags, body); err == nil {
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
	if p.clientID != "dev-1" || !p.cleanSession || p.keepAlive != 60 || p.username != "user" || string(p.password) != "secret" ||
		p.will == nil || p.will.Topic != "dev/1/status" || string(p.will.Payload) != "offline" || p.will.QoS != 1 {
		t.Fatalf("%+v", p)
	}
	v5 := appendString(nil, "MQTT")
	v5 = append(v5, 5, 0x02, 0, 0)
	if _, err := parseConnect(0, v5); !errors.Is(err, errBadProtocol) {
		t.Fatalf("MQTT 5 CONNECT: %v", err)
	}
	reserved := appendString(nil, "MQTT")
	reserved = append(reserved, 4, 0x03, 0, 0) // reserved flag set
	reserved = appendString(reserved, "x")
	if _, err := parseConnect(0, reserved); err == nil {
		t.Fatal("reserved connect flag accepted")
	}
}
