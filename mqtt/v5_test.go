//go:build linux
// +build linux

package mqtt

import (
	"bytes"
	"testing"
	"time"
)

func v5(o connectOpts) connectOpts { o.level = version5; return o }

func withProps(f func(w *propWriter)) *propWriter {
	w := &propWriter{}
	f(w)
	return w
}

func TestV5Connack(t *testing.T) {
	_, addr := startBroker(t, Config{SessionExpiry: 10 * time.Second, MaxInflight: 32, TopicAliasMaximum: 8})
	c, present, code := dial(t, addr, "", v5(connectOpts{props: withProps(func(w *propWriter) {
		w.uint32(propSessionExpiry, 0xFFFFFFFF) // never: capped to SessionExpiry
	})}))
	if present || code != rcSuccess {
		t.Fatalf("present=%v code=%#x", present, code)
	}
	p := c.connack
	if p.has[propAssignedClientID] != true || p.sessionExpiry != 10 || p.receiveMaximum != 32 || p.topicAliasMaximum != 8 ||
		p.maximumPacketSize != 1<<20 || !p.has[propSubIDAvailable] || !p.has[propSharedSubAvailable] || p.has[propMaximumQoS] {
		t.Fatalf("CONNACK properties %+v", p)
	}
	// A session expiry within the limit is not echoed
	c2, _, _ := dial(t, addr, "x", v5(connectOpts{props: withProps(func(w *propWriter) { w.uint32(propSessionExpiry, 5) })}))
	if c2.connack.has[propSessionExpiry] || c2.connack.has[propAssignedClientID] {
		t.Fatalf("CONNACK properties %+v", c2.connack)
	}
	// Enhanced authentication is refused
	_, _, code = dial(t, addr, "y", v5(connectOpts{props: withProps(func(w *propWriter) { w.string(propAuthMethod, "SCRAM-SHA-1") })}))
	if code != rcBadAuthMethod {
		t.Fatalf("auth method: code %#x", code)
	}
	// MQTT 5 reason codes for refused connections
	_, addr2 := startBroker(t, Config{Authenticate: func(string, string, []byte) bool { return false }})
	bad, _, code := dial(t, addr2, "z", v5(connectOpts{clean: true, user: "u", pass: "p"}))
	if code != rcBadCredentials {
		t.Fatalf("code %#x", code)
	}
	bad.expectClosed()
}

// Messages between MQTT 5 and MQTT 3.1.1 clients, with properties forwarded to MQTT 5 subscribers only.
func TestV5Interop(t *testing.T) {
	got := make(chan *Properties, 1)
	b, addr := startBroker(t, Config{OnPublish: func(m *Message) {
		if m.Topic == "data/1" {
			got <- m.Properties
		}
	}})
	sub5 := connect(t, addr, "sub5", v5(connectOpts{clean: true}))
	sub5.subscribe(1, "data/#", 1)
	sub3 := connect(t, addr, "sub3", connectOpts{clean: true})
	sub3.subscribe(1, "data/#", 1)
	pub5 := connect(t, addr, "pub5", v5(connectOpts{clean: true}))

	mp := &Properties{PayloadFormat: 1, ContentType: "application/json", ResponseTopic: "reply/pub5",
		CorrelationData: []byte("req-1"), User: []UserProperty{{"model", "x9"}, {"fw", "1.2"}}}
	pub5.send(appendPublish(nil, version5, "data/1", []byte(`{"t":1}`), 1, false, false, 1, mp, 60))
	pub5.expectAck(typePuback, 1)
	if ap := <-got; ap == nil || ap.ContentType != "application/json" || len(ap.User) != 2 {
		t.Fatalf("OnPublish properties %+v", ap)
	}

	p := sub5.expectPublish(true)
	fwd := p.props.messageProps()
	if string(p.payload) != `{"t":1}` || fwd == nil || fwd.ContentType != mp.ContentType || fwd.ResponseTopic != mp.ResponseTopic ||
		!bytes.Equal(fwd.CorrelationData, mp.CorrelationData) || len(fwd.User) != 2 || fwd.User[0] != mp.User[0] || fwd.PayloadFormat != 1 {
		t.Fatalf("MQTT 5 subscriber got %+v %+v", p, fwd)
	}
	if e := p.props.messageExpiry; e == 0 || e > 60 {
		t.Fatalf("forwarded message expiry %d", e)
	}
	if p := sub3.expectPublish(true); string(p.payload) != `{"t":1}` {
		t.Fatalf("MQTT 3.1.1 subscriber got %+v", p)
	}

	// MQTT 3.1.1 publisher to MQTT 5 subscriber, QoS 0 (one encoding per protocol version)
	pub3 := connect(t, addr, "pub3", connectOpts{clean: true})
	pub3.publish("data/2", "plain", 0, false, 0)
	if p := sub5.expectPublish(true); string(p.payload) != "plain" || p.props.messageProps() != nil || p.props.messageExpiry != 0 {
		t.Fatalf("%+v", p)
	}
	if p := sub3.expectPublish(true); string(p.payload) != "plain" {
		t.Fatalf("%+v", p)
	}

	// The application publishes with properties
	if err := b.PublishMessage(&Message{Topic: "data/3", Payload: []byte("cmd"), QoS: 1,
		Properties: &Properties{User: []UserProperty{{"src", "app"}}}}); err != nil {
		t.Fatal(err)
	}
	if p := sub5.expectPublish(true); p.props.messageProps() == nil || p.props.user[0].Value != "app" {
		t.Fatalf("%+v", p)
	}
	sub3.expectPublish(true)

	// Unsubscribe reports whether a subscription existed
	sub5.send(packet(nil, typeUnsubscribe<<4|0x02, appendString(append([]byte{0, 7}, 0), "data/#")))
	sub5.send(packet(nil, typeUnsubscribe<<4|0x02, appendString(append([]byte{0, 8}, 0), "data/#")))
	for _, want := range []struct {
		id   uint16
		code byte
	}{{7, rcSuccess}, {8, rcNoSubscriptionExisted}} {
		typ, _, body := sub5.read()
		if typ != typeUnsuback || string(body) != string([]byte{0, byte(want.id), 0, want.code}) {
			t.Fatalf("UNSUBACK % x, want id %d code %#x", body, want.id, want.code)
		}
	}
}

func TestSessionExpiry(t *testing.T) {
	b, addr := startBroker(t, Config{SessionExpiry: time.Second})
	// MQTT 3.1.1 persistent session: removed after SessionExpiry offline
	c := connect(t, addr, "dev3", connectOpts{})
	c.subscribe(1, "a", 1)
	c.send([]byte{typeDisconnect << 4, 0})
	c.expectClosed()
	time.Sleep(300 * time.Millisecond)
	_, present, _ := dial(t, addr, "dev3", connectOpts{}) // back in time: kept
	if !present {
		t.Fatal("session lost before its expiry")
	}
	time.Sleep(100 * time.Millisecond) // the connection above closes at the end of the test only
	if st := b.Stats(); st.SessionsExpired != 0 || st.Sessions != 1 {
		t.Fatalf("%+v", st)
	}

	// MQTT 5: the session expiry the client asks for (0 ends the session with the connection)
	short := connect(t, addr, "dev5", v5(connectOpts{props: withProps(func(w *propWriter) { w.uint32(propSessionExpiry, 1) })}))
	short.subscribe(1, "a", 1)
	short.c.Close()
	zero := connect(t, addr, "dev0", v5(connectOpts{}))
	zero.subscribe(1, "a", 1)
	zero.c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for b.Stats().SessionsExpired != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("session not expired: %+v", b.Stats())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st := b.Stats(); st.Sessions != 1 || st.Subscriptions != 1 { // only dev3, connected
		t.Fatalf("%+v", st)
	}
	_, present, _ = dial(t, addr, "dev5", v5(connectOpts{}))
	if present {
		t.Fatal("expired session still present")
	}

	// The expiry can be changed on DISCONNECT (but not from 0)
	d := connect(t, addr, "dev5b", v5(connectOpts{props: withProps(func(w *propWriter) { w.uint32(propSessionExpiry, 60) })}))
	d.send(packet(nil, typeDisconnect<<4, withProps(func(w *propWriter) { w.uint32(propSessionExpiry, 0) }).appendTo([]byte{0})))
	d.expectClosed()
	time.Sleep(100 * time.Millisecond)
	if _, present, _ := dial(t, addr, "dev5b", v5(connectOpts{})); present {
		t.Fatal("session kept after DISCONNECT with session expiry 0")
	}
	z := connect(t, addr, "dev0b", v5(connectOpts{}))
	z.send(packet(nil, typeDisconnect<<4, withProps(func(w *propWriter) { w.uint32(propSessionExpiry, 60) }).appendTo([]byte{0})))
	z.expectDisconnect(rcProtocolError)
}

func TestMessageExpiry(t *testing.T) {
	b, addr := startBroker(t, Config{})
	opts := v5(connectOpts{props: withProps(func(w *propWriter) { w.uint32(propSessionExpiry, 60) })})
	sub := connect(t, addr, "sub", opts)
	sub.subscribe(1, "cmd", 1)
	sub.send([]byte{typeDisconnect << 4, 0})
	sub.expectClosed()

	pub := connect(t, addr, "pub", v5(connectOpts{clean: true}))
	pub.send(appendPublish(nil, version5, "cmd", []byte("stale"), 1, false, false, 1, nil, 1))
	pub.expectAck(typePuback, 1)
	pub.send(appendPublish(nil, version5, "cmd", []byte("fresh"), 1, false, false, 2, nil, 60))
	pub.expectAck(typePuback, 2)
	// A retained message that expires
	pub.send(appendPublish(nil, version5, "cfg", []byte("old"), 0, true, false, 0, nil, 1))
	time.Sleep(1100 * time.Millisecond)

	sub, present, _ := dial(t, addr, "sub", opts)
	if !present {
		t.Fatal("session not present")
	}
	p := sub.expectPublish(true)
	if string(p.payload) != "fresh" || p.props.messageExpiry == 0 || p.props.messageExpiry > 59 {
		t.Fatalf("got %q expiry %d", p.payload, p.props.messageExpiry)
	}
	sub.expectNothing(200 * time.Millisecond)
	sub.subscribe(2, "cfg", 0)
	sub.expectNothing(200 * time.Millisecond)
	if st := b.Stats(); st.MessagesExpired != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestTopicAlias(t *testing.T) {
	_, addr := startBroker(t, Config{TopicAliasMaximum: 4})
	sub := connect(t, addr, "sub", connectOpts{clean: true})
	sub.subscribe(1, "#", 0)
	pub := connect(t, addr, "pub", v5(connectOpts{clean: true}))
	alias := func(topic string, a uint16, payload string) []byte {
		w := withProps(func(w *propWriter) { w.uint16(propTopicAlias, a) })
		body := appendString(nil, topic)
		body = w.appendTo(body)
		return packet(nil, typePublish<<4, append(body, payload...))
	}
	pub.send(alias("factory/line-1/temperature", 1, "a"))
	pub.send(alias("", 1, "b"))
	for _, want := range []string{"a", "b"} {
		if p := sub.expectPublish(false); p.topic != "factory/line-1/temperature" || string(p.payload) != want {
			t.Fatalf("%+v", p)
		}
	}
	pub.send(alias("", 2, "c")) // never set
	pub.expectDisconnect(rcProtocolError)
	pub2 := connect(t, addr, "pub2", v5(connectOpts{clean: true}))
	pub2.send(alias("t", 5, "d")) // above TopicAliasMaximum
	pub2.expectDisconnect(rcTopicAliasInvalid)
}

func TestReceiveMaximumAndPacketSize(t *testing.T) {
	_, addr := startBroker(t, Config{})
	sub := connect(t, addr, "sub", v5(connectOpts{clean: true, props: withProps(func(w *propWriter) {
		w.uint16(propReceiveMaximum, 2)
		w.uint32(propMaximumPacketSize, 200)
	})}))
	sub.subscribe(1, "t", 1)
	pub := connect(t, addr, "pub", connectOpts{clean: true})
	pub.publish("t", string(make([]byte, 300)), 1, false, 99) // too large for the subscriber: dropped
	pub.publish("t", string(make([]byte, 300)), 0, false, 0)
	for i := 1; i <= 4; i++ {
		pub.publish("t", "m", 1, false, uint16(i))
	}
	// Only 2 unacknowledged at a time
	p1 := sub.expectPublish(false)
	sub.expectPublish(false)
	sub.expectNothing(200 * time.Millisecond)
	sub.send(appendAck(nil, version5, typePuback, p1.id, rcSuccess))
	if p := sub.expectPublish(false); string(p.payload) != "m" { // the third, now that a slot is free
		t.Fatalf("%+v", p)
	}
	sub.expectNothing(200 * time.Millisecond)
}

func TestSubscriptionOptions(t *testing.T) {
	_, addr := startBroker(t, Config{})
	c := connect(t, addr, "self", v5(connectOpts{clean: true}))
	c.subscribeOpts(1, "chat", 0x01|0x04, nil) // QoS 1, no local
	c.publish("chat", "mine", 1, false, 1)
	c.expectNothing(200 * time.Millisecond)
	other := connect(t, addr, "other", connectOpts{clean: true})
	other.publish("chat", "theirs", 0, true, 0)
	if p := c.expectPublish(true); string(p.payload) != "theirs" || p.retain {
		t.Fatalf("%+v", p)
	}

	// Retain as published keeps the retain flag on live messages
	c.subscribeOpts(2, "state", 0x08, nil)
	other.publish("state", "on", 0, true, 0)
	if p := c.expectPublish(true); !p.retain {
		t.Fatalf("retain as published: %+v", p)
	}

	// Retain handling: 0 always sends retained messages, 1 only for a new subscription, 2 never
	r := connect(t, addr, "r", v5(connectOpts{clean: true}))
	r.subscribeOpts(1, "state", 0x20, nil)
	r.expectNothing(200 * time.Millisecond)
	r.subscribeOpts(2, "chat", 0x10, nil)
	if p := r.expectPublish(true); string(p.payload) != "theirs" || !p.retain {
		t.Fatalf("%+v", p)
	}
	r.subscribeOpts(3, "chat", 0x10, nil) // not new
	r.expectNothing(200 * time.Millisecond)
	r.subscribeOpts(4, "chat", 0x00, nil)
	r.expectPublish(true)

	// Shared subscriptions and subscription identifiers are not supported
	if code := r.subscribeOpts(5, "$share/g/chat", 1, nil); code != rcSharedSubNotSupported {
		t.Fatalf("shared subscription: %#x", code)
	}
	if code := other.subscribe(5, "$share/g/chat", 1); code != rcUnspecified {
		t.Fatalf("MQTT 3.1.1 shared subscription: %#x", code)
	}
	r.send(packet(nil, typeSubscribe<<4|0x02, append(withProps(func(w *propWriter) {
		w.b = append(w.b, propSubscriptionID, 7)
	}).appendTo([]byte{0, 6}), append(appendString(nil, "x"), 0)...)))
	r.expectDisconnect(rcSubIDsNotSupported)
}

func TestWillDelay(t *testing.T) {
	_, addr := startBroker(t, Config{})
	sub := connect(t, addr, "sub", connectOpts{clean: true})
	sub.subscribe(1, "status/#", 0)
	will := &Message{Topic: "status/dev", Payload: []byte("offline")}
	opts := v5(connectOpts{will: will,
		props:     withProps(func(w *propWriter) { w.uint32(propSessionExpiry, 60) }),
		willProps: withProps(func(w *propWriter) { w.uint32(propWillDelay, 1) })})

	// Reconnecting within the delay cancels the will
	dev := connect(t, addr, "dev", opts)
	dev.c.Close()
	time.Sleep(300 * time.Millisecond)
	dev = connect(t, addr, "dev", opts)
	sub.expectNothing(1200 * time.Millisecond)

	// Otherwise it is published after the delay
	dev.c.Close()
	start := time.Now()
	if p := sub.expectPublish(false); string(p.payload) != "offline" {
		t.Fatalf("%+v", p)
	}
	if d := time.Since(start); d < 800*time.Millisecond {
		t.Fatalf("will published after %v, want about 1s", d)
	}

	// The will is published at the end of the session if that comes first, and on DISCONNECT with
	// reason 0x04 (disconnect with will message)
	quick := connect(t, addr, "quick", v5(connectOpts{will: will,
		willProps: withProps(func(w *propWriter) { w.uint32(propWillDelay, 30) })}))
	quick.c.Close()
	sub.expectPublish(false)
	polite := connect(t, addr, "polite", v5(connectOpts{clean: true, will: will}))
	polite.send([]byte{typeDisconnect << 4, 1, rcDisconnectWithWill})
	polite.expectClosed()
	sub.expectPublish(false)
}

func TestV5ServerDisconnects(t *testing.T) {
	_, addr := startBroker(t, Config{AuthorizePublish: func(_, topic string) bool { return topic != "secret" }})
	// Session taken over
	first := connect(t, addr, "dev", v5(connectOpts{clean: true}))
	connect(t, addr, "dev", v5(connectOpts{clean: true}))
	first.expectDisconnect(rcSessionTakenOver)
	// Keepalive timeout
	idle := connect(t, addr, "idle", v5(connectOpts{clean: true, keepAlive: 1}))
	idle.expectDisconnect(rcKeepAliveTimeout)
	// Unauthorized publish: reason code in the acknowledgement
	pub := connect(t, addr, "pub", v5(connectOpts{clean: true}))
	pub.send(appendPublish(nil, version5, "secret", []byte("x"), 1, false, false, 1, nil, 0))
	pub.expectAckReason(typePuback, 1, rcNotAuthorized)
	pub.send(appendPublish(nil, version5, "secret", []byte("x"), 2, false, false, 2, nil, 0))
	pub.expectAckReason(typePubrec, 2, rcNotAuthorized)
	pub.send(appendAck(nil, version5, typePubrel, 2, rcSuccess))
	pub.expectAckReason(typePubcomp, 2, rcPacketIDNotFound)
	// Wildcards in a topic name
	pub.send(appendPublish(nil, version5, "a/+", nil, 0, false, false, 0, nil, 0))
	pub.expectDisconnect(rcTopicNameInvalid)
}
