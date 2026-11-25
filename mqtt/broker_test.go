//go:build linux
// +build linux

package mqtt

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/y001j/uringnet"
)

// startBroker serves a broker on a random port.
func startBroker(t *testing.T, cfg Config, opts ...uringnet.Option) (*Broker, string) {
	t.Helper()
	b := NewBroker(cfg)
	base := []uringnet.Option{uringnet.WithRings(2)}
	if os.Getenv("URINGNET_BACKEND") == "epoll" { // as in the uringnet package tests
		base = append(base, uringnet.WithBackend(uringnet.BackendEpoll))
	}
	srv, err := uringnet.Listen("tcp4", "127.0.0.1:0", b.Handler(), append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("URINGNET_BACKEND") == "epoll" && srv.Backend() != uringnet.BackendEpoll {
		t.Fatal("not on the epoll backend")
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close(); b.Close() })
	return b, srv.Addr().String()
}

// tclient is a minimal MQTT client built on the package's codec.
type tclient struct {
	t       *testing.T
	c       net.Conn
	rd      *bufio.Reader
	v       byte  // protocol level
	connack props // MQTT 5 CONNACK properties
}

type connectOpts struct {
	clean      bool
	keepAlive  uint16
	will       *Message
	user, pass string
	level      byte
	props      *propWriter // MQTT 5 CONNECT properties
	willProps  *propWriter // MQTT 5 will properties
}

func connectPkt(id string, o connectOpts) []byte {
	level := o.level
	if level == 0 {
		level = 4
	}
	flags := byte(0)
	if o.clean {
		flags |= 0x02
	}
	if o.will != nil {
		flags |= 0x04 | o.will.QoS<<3
		if o.will.Retain {
			flags |= 0x20
		}
	}
	if o.user != "" {
		flags |= 0x80 | 0x40
	}
	body := appendString(nil, "MQTT")
	body = append(body, level, flags)
	body = binary.BigEndian.AppendUint16(body, o.keepAlive)
	if level == version5 {
		if o.props == nil {
			o.props = &propWriter{}
		}
		body = o.props.appendTo(body)
	}
	body = appendString(body, id)
	if o.will != nil {
		if level == version5 {
			if o.willProps == nil {
				o.willProps = &propWriter{}
			}
			body = o.willProps.appendTo(body)
		}
		body = appendString(body, o.will.Topic)
		body = appendBytes(body, o.will.Payload)
	}
	if o.user != "" {
		body = appendString(body, o.user)
		body = appendBytes(body, []byte(o.pass))
	}
	return append(appendLength([]byte{typeConnect << 4}, len(body)), body...)
}

// dial connects and returns the client with the CONNACK's session-present flag and return code.
func dial(t *testing.T, addr, id string, o connectOpts) (*tclient, bool, byte) {
	t.Helper()
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	tc := &tclient{t: t, c: c, rd: bufio.NewReader(c), v: o.level}
	if tc.v == 0 {
		tc.v = version311
	}
	t.Cleanup(func() { c.Close() })
	tc.send(connectPkt(id, o))
	typ, _, body := tc.read()
	if typ != typeConnack || len(body) < 2 || (tc.v != version5 && len(body) != 2) {
		t.Fatalf("expected CONNACK, got type %d % x", typ, body)
	}
	if tc.v == version5 {
		r := &reader{b: body[2:]}
		pp, err := parseProps(r)
		if err != nil || len(r.b) != 0 {
			t.Fatalf("CONNACK properties % x: %v", body, err)
		}
		tc.connack = pp
	}
	return tc, body[0] == 1, body[1]
}

func connect(t *testing.T, addr, id string, o connectOpts) *tclient {
	t.Helper()
	tc, _, code := dial(t, addr, id, o)
	if code != connAccepted {
		t.Fatalf("CONNACK code %d", code)
	}
	return tc
}

func (tc *tclient) send(pkt []byte) {
	tc.t.Helper()
	if _, err := tc.c.Write(pkt); err != nil {
		tc.t.Fatal(err)
	}
}

func (tc *tclient) readErr(d time.Duration) (typ, flags byte, body []byte, err error) {
	tc.c.SetReadDeadline(time.Now().Add(d))
	b0, err := tc.rd.ReadByte()
	if err != nil {
		return 0, 0, nil, err
	}
	var lenBytes []byte
	for {
		b, err := tc.rd.ReadByte()
		if err != nil {
			return 0, 0, nil, err
		}
		lenBytes = append(lenBytes, b)
		if b&0x80 == 0 {
			break
		}
	}
	n, _, _ := decodeLength(lenBytes)
	body = make([]byte, n)
	if _, err := io.ReadFull(tc.rd, body); err != nil {
		return 0, 0, nil, err
	}
	return b0 >> 4, b0 & 0x0f, body, nil
}

func (tc *tclient) read() (typ, flags byte, body []byte) {
	tc.t.Helper()
	typ, flags, body, err := tc.readErr(5 * time.Second)
	if err != nil {
		tc.t.Fatalf("read: %v", err)
	}
	return typ, flags, body
}

func (tc *tclient) subscribe(id uint16, filter string, qos byte) byte {
	tc.t.Helper()
	return tc.subscribeOpts(id, filter, qos, nil)
}

// subscribeOpts subscribes with an options byte (MQTT 5 flags included) and SUBSCRIBE properties.
func (tc *tclient) subscribeOpts(id uint16, filter string, opts byte, pw *propWriter) byte {
	tc.t.Helper()
	body := binary.BigEndian.AppendUint16(nil, id)
	if tc.v == version5 {
		if pw == nil {
			pw = &propWriter{}
		}
		body = pw.appendTo(body)
	}
	body = appendString(body, filter)
	body = append(body, opts)
	tc.send(packet(nil, typeSubscribe<<4|0x02, body))
	typ, _, resp := tc.read()
	want := 3
	if tc.v == version5 {
		want = 4 // with an empty property list
	}
	if typ != typeSuback || len(resp) != want || binary.BigEndian.Uint16(resp) != id {
		tc.t.Fatalf("expected SUBACK %d, got type %d % x", id, typ, resp)
	}
	return resp[want-1]
}

func (tc *tclient) publish(topic, payload string, qos byte, retain bool, id uint16) {
	tc.t.Helper()
	tc.send(appendPublish(nil, tc.v, topic, []byte(payload), qos, retain, false, id, nil, 0))
	switch qos {
	case 1:
		tc.expectAck(typePuback, id)
	case 2:
		tc.expectAck(typePubrec, id)
	}
}

func (tc *tclient) expectAck(typ byte, id uint16) {
	tc.t.Helper()
	tc.expectAckReason(typ, id, rcSuccess)
}

// expectAckReason expects an acknowledgement with an MQTT 5 reason code (0: none sent).
func (tc *tclient) expectAckReason(typ byte, id uint16, reason byte) {
	tc.t.Helper()
	got, _, body := tc.read()
	want := []byte{byte(id >> 8), byte(id)}
	if reason != rcSuccess {
		want = append(want, reason)
	}
	if got != typ || string(body) != string(want) {
		tc.t.Fatalf("expected ack type %d % x, got type %d % x", typ, want, got, body)
	}
}

// expectDisconnect expects an MQTT 5 DISCONNECT with reason, then the connection to close.
func (tc *tclient) expectDisconnect(reason byte) {
	tc.t.Helper()
	typ, _, body := tc.read()
	if typ != typeDisconnect || len(body) < 1 || body[0] != reason {
		tc.t.Fatalf("expected DISCONNECT %#x, got type %d % x", reason, typ, body)
	}
	tc.expectClosed()
}

// expectPublish reads a PUBLISH and returns it (and acknowledges QoS 1 if ack is set).
func (tc *tclient) expectPublish(ack bool) *publishPacket {
	tc.t.Helper()
	typ, flags, body := tc.read()
	if typ != typePublish {
		tc.t.Fatalf("expected PUBLISH, got type %d", typ)
	}
	p, err := parsePublish(flags, body, tc.v)
	if err != nil {
		tc.t.Fatal(err)
	}
	p.payload = append([]byte(nil), p.payload...)
	if ack && p.qos == 1 {
		tc.send(appendAck(nil, tc.v, typePuback, p.id, rcSuccess))
	}
	return p
}

func (tc *tclient) expectNothing(d time.Duration) {
	tc.t.Helper()
	if typ, _, body, err := tc.readErr(d); err == nil {
		tc.t.Fatalf("unexpected packet type %d % x", typ, body)
	}
}

func (tc *tclient) expectClosed() {
	tc.t.Helper()
	for {
		_, _, _, err := tc.readErr(5 * time.Second)
		if err == nil {
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			tc.t.Fatal("connection not closed by the broker")
		}
		return
	}
}

func TestConnectPing(t *testing.T) {
	_, addr := startBroker(t, Config{})
	c, present, code := dial(t, addr, "dev-1", connectOpts{clean: true})
	if present || code != connAccepted {
		t.Fatalf("CONNACK present=%v code=%d", present, code)
	}
	c.send([]byte{typePingreq << 4, 0})
	if typ, _, _ := c.read(); typ != typePingresp {
		t.Fatalf("got type %d, want PINGRESP", typ)
	}
	c.send([]byte{typeDisconnect << 4, 0})
	c.expectClosed()
}

func TestPublishSubscribe(t *testing.T) {
	b, addr := startBroker(t, Config{})
	sub := connect(t, addr, "sub", connectOpts{clean: true})
	if code := sub.subscribe(1, "sensors/+/temp", 1); code != 1 {
		t.Fatalf("granted %d", code)
	}
	if code := sub.subscribe(2, "alarms/#", 2); code != 1 {
		t.Fatalf("QoS 2 subscription granted %d, want 1", code)
	}
	pub := connect(t, addr, "pub", connectOpts{clean: true})

	pub.publish("sensors/a/temp", "21.5", 0, false, 0)
	if p := sub.expectPublish(true); p.topic != "sensors/a/temp" || string(p.payload) != "21.5" || p.qos != 0 {
		t.Fatalf("%+v", p)
	}
	pub.publish("alarms/fire/room1", "!", 1, false, 5)
	if p := sub.expectPublish(true); p.topic != "alarms/fire/room1" || p.qos != 1 || p.id == 0 || p.retain {
		t.Fatalf("%+v", p)
	}
	pub.publish("sensors/a/humidity", "40", 1, false, 6) // matches no filter
	sub.expectNothing(200 * time.Millisecond)

	// Unsubscribe stops delivery
	body := binary.BigEndian.AppendUint16(nil, 9)
	body = appendString(body, "sensors/+/temp")
	sub.send(append(appendLength([]byte{typeUnsubscribe<<4 | 0x02}, len(body)), body...))
	sub.expectAck(typeUnsuback, 9)
	pub.publish("sensors/a/temp", "22", 0, false, 0)
	sub.expectNothing(200 * time.Millisecond)
	if st := b.Stats(); st.Clients != 2 || st.Subscriptions != 1 || st.MessagesReceived != 4 {
		t.Fatalf("stats %+v", st)
	}
}

func TestRetained(t *testing.T) {
	_, addr := startBroker(t, Config{})
	pub := connect(t, addr, "pub", connectOpts{clean: true})
	pub.publish("cfg/interval", "60", 1, true, 1)
	pub.publish("cfg/mode", "eco", 0, true, 0)

	sub := connect(t, addr, "sub", connectOpts{clean: true})
	sub.subscribe(1, "cfg/#", 1)
	got := map[string]string{}
	for i := 0; i < 2; i++ {
		p := sub.expectPublish(true)
		if !p.retain {
			t.Fatalf("retained message without the retain flag: %+v", p)
		}
		got[p.topic] = string(p.payload)
	}
	if got["cfg/interval"] != "60" || got["cfg/mode"] != "eco" {
		t.Fatalf("retained %v", got)
	}
	// Live messages to existing subscribers have retain cleared; an empty retained message clears the topic
	pub.publish("cfg/mode", "", 0, true, 0)
	if p := sub.expectPublish(true); p.retain || len(p.payload) != 0 {
		t.Fatalf("%+v", p)
	}
	late := connect(t, addr, "late", connectOpts{clean: true})
	late.subscribe(1, "cfg/mode", 0)
	late.expectNothing(200 * time.Millisecond)
}

func TestWill(t *testing.T) {
	_, addr := startBroker(t, Config{})
	sub := connect(t, addr, "sub", connectOpts{clean: true})
	sub.subscribe(1, "dev/+/status", 1)

	will := &Message{Topic: "dev/1/status", Payload: []byte("offline"), QoS: 1}
	dev := connect(t, addr, "dev-1", connectOpts{clean: true, will: will})
	dev.c.Close() // connection lost without DISCONNECT
	if p := sub.expectPublish(true); p.topic != "dev/1/status" || string(p.payload) != "offline" {
		t.Fatalf("%+v", p)
	}
	polite := connect(t, addr, "dev-2", connectOpts{clean: true, will: &Message{Topic: "dev/2/status", Payload: []byte("offline")}})
	polite.send([]byte{typeDisconnect << 4, 0})
	polite.expectClosed()
	sub.expectNothing(300 * time.Millisecond)
}

func TestPersistentSession(t *testing.T) {
	_, addr := startBroker(t, Config{})
	sub, present, _ := dial(t, addr, "sensor-gw", connectOpts{clean: false})
	if present {
		t.Fatal("session present on first connect")
	}
	sub.subscribe(1, "cmd/#", 1)
	sub.send([]byte{typeDisconnect << 4, 0})
	sub.expectClosed()

	pub := connect(t, addr, "pub", connectOpts{clean: true})
	for i, m := range []string{"one", "two", "three"} {
		pub.publish("cmd/x", m, 1, false, uint16(i+1))
	}
	pub.publish("cmd/x", "lost", 0, false, 0) // QoS 0 is not queued for offline sessions

	// Reconnect: the session is present and the queued messages arrive, in order
	sub, present, _ = dial(t, addr, "sensor-gw", connectOpts{clean: false})
	if !present {
		t.Fatal("session not present after reconnect")
	}
	for _, want := range []string{"one", "two"} {
		if p := sub.expectPublish(true); string(p.payload) != want {
			t.Fatalf("got %q, want %q", p.payload, want)
		}
	}
	// Receive "three" without acknowledging it, drop the connection: it is resent with DUP
	if p := sub.expectPublish(false); string(p.payload) != "three" {
		t.Fatalf("got %q", p.payload)
	}
	sub.c.Close()
	time.Sleep(100 * time.Millisecond)
	sub, _, _ = dial(t, addr, "sensor-gw", connectOpts{clean: false})
	if p := sub.expectPublish(true); string(p.payload) != "three" || !p.dup {
		t.Fatalf("resend: %+v", p)
	}
	sub.expectNothing(200 * time.Millisecond)

	// A clean session discards the stored one
	sub.send([]byte{typeDisconnect << 4, 0})
	sub.expectClosed()
	_, present, _ = dial(t, addr, "sensor-gw", connectOpts{clean: true})
	if present {
		t.Fatal("clean session reported a present session")
	}
}

func TestQoS2Incoming(t *testing.T) {
	_, addr := startBroker(t, Config{})
	sub := connect(t, addr, "sub", connectOpts{clean: true})
	sub.subscribe(1, "t", 1)
	pub := connect(t, addr, "pub", connectOpts{clean: true})
	pub.publish("t", "once", 2, false, 7)
	pub.send(appendPublish(nil, version311, "t", []byte("once"), 2, false, true, 7, nil, 0)) // resent before PUBREL
	pub.expectAck(typePubrec, 7)
	pub.send(appendAck(nil, version311, typePubrel, 7, rcSuccess))
	pub.expectAck(typePubcomp, 7)
	if p := sub.expectPublish(true); string(p.payload) != "once" {
		t.Fatalf("%+v", p)
	}
	sub.expectNothing(200 * time.Millisecond) // delivered exactly once
}

func TestSessionTakeover(t *testing.T) {
	var disconnects atomic.Int32
	_, addr := startBroker(t, Config{OnDisconnect: func(string, error) { disconnects.Add(1) }})
	first := connect(t, addr, "dev", connectOpts{clean: false})
	first.subscribe(1, "a", 1)
	second, present, _ := dial(t, addr, "dev", connectOpts{clean: false})
	if !present {
		t.Fatal("takeover lost the session")
	}
	first.expectClosed()
	// The new connection owns the session: it receives messages for the old subscription
	pub := connect(t, addr, "pub", connectOpts{clean: true})
	pub.publish("a", "hi", 1, false, 1)
	if p := second.expectPublish(true); string(p.payload) != "hi" {
		t.Fatalf("%+v", p)
	}
	// The old connection's OnClose runs on its own event loop, possibly after the client saw the
	// close: wait for it, then check that it is reported once
	deadline := time.Now().Add(2 * time.Second)
	for disconnects.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := disconnects.Load(); n != 1 {
		t.Fatalf("%d disconnects", n)
	}
}

func TestKeepAliveTimeout(t *testing.T) {
	_, addr := startBroker(t, Config{})
	sub := connect(t, addr, "sub", connectOpts{clean: true})
	sub.subscribe(1, "will/#", 0)
	idle := connect(t, addr, "idle", connectOpts{clean: true, keepAlive: 1, will: &Message{Topic: "will/idle", Payload: []byte("gone")}})
	start := time.Now()
	idle.expectClosed()
	if d := time.Since(start); d < time.Second || d > 3*time.Second {
		t.Fatalf("closed after %v, want about 1.5s", d)
	}
	if p := sub.expectPublish(false); p.topic != "will/idle" {
		t.Fatalf("%+v", p)
	}
}

func TestAuthentication(t *testing.T) {
	_, addr := startBroker(t, Config{Authenticate: func(id, user string, pass []byte) bool {
		return user == "admin" && string(pass) == "s3cret"
	}})
	ok := connect(t, addr, "a", connectOpts{clean: true, user: "admin", pass: "s3cret"})
	ok.send([]byte{typePingreq << 4, 0})
	ok.read()
	bad, _, code := dial(t, addr, "b", connectOpts{clean: true, user: "admin", pass: "nope"})
	if code != connBadCredentials {
		t.Fatalf("code %d", code)
	}
	bad.expectClosed()
}

func TestUnsupportedProtocol(t *testing.T) {
	_, addr := startBroker(t, Config{})
	// MQTT 3 (level 3 would be accepted as "MQIsdp" only) and MQTT 6 get a 3.1.1 CONNACK with code 1
	c, _, code := dial(t, addr, "v6", connectOpts{clean: true, level: 6})
	if code != connBadProtocol {
		t.Fatalf("code %d", code)
	}
	c.expectClosed()
	// A first packet other than CONNECT closes the connection
	raw, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.Write([]byte{typePingreq << 4, 0})
	(&tclient{t: t, c: raw, rd: bufio.NewReader(raw)}).expectClosed()
}

func TestApplicationHooks(t *testing.T) {
	got := make(chan Message, 4)
	b, addr := startBroker(t, Config{
		OnPublish: func(m *Message) {
			got <- Message{Topic: m.Topic, Payload: append([]byte(nil), m.Payload...), ClientID: m.ClientID, QoS: m.QoS}
		},
		AuthorizePublish: func(id, topic string) bool { return topic != "forbidden" },
	})
	dev := connect(t, addr, "dev-7", connectOpts{clean: true})
	dev.subscribe(1, "down/#", 1)
	dev.publish("up/telemetry", `{"t":21}`, 1, false, 1)
	m := <-got
	if m.ClientID != "dev-7" || m.Topic != "up/telemetry" || string(m.Payload) != `{"t":21}` || m.QoS != 1 {
		t.Fatalf("OnPublish got %+v", m)
	}
	dev.publish("forbidden", "x", 1, false, 2) // acknowledged but dropped
	select {
	case m := <-got:
		t.Fatalf("unauthorized message reached OnPublish: %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
	// The application publishes to devices
	if err := b.Publish("down/cmd", []byte("reboot"), 1, false); err != nil {
		t.Fatal(err)
	}
	if p := dev.expectPublish(true); p.topic != "down/cmd" || string(p.payload) != "reboot" {
		t.Fatalf("%+v", p)
	}
	if err := b.Publish("bad/#", nil, 0, false); !errors.Is(err, ErrInvalidTopic) {
		t.Fatalf("Publish to a wildcard topic: %v", err)
	}
}

// Fan-out to subscribers on every event loop (direct writes and AsyncWrite).
func TestFanOutAcrossRings(t *testing.T) {
	_, addr := startBroker(t, Config{}, uringnet.WithRings(4))
	const n = 40
	subs := make([]*tclient, n)
	for i := range subs {
		subs[i] = connect(t, addr, "s"+string(rune('A'+i%26))+string(rune('a'+i/26)), connectOpts{clean: true})
		subs[i].subscribe(1, "broadcast", 1)
	}
	pub := connect(t, addr, "pub", connectOpts{clean: true})
	for k := 0; k < 20; k++ {
		pub.publish("broadcast", "msg", byte(k%2), false, uint16(k+1))
	}
	var wg sync.WaitGroup
	for _, s := range subs {
		wg.Add(1)
		go func(s *tclient) {
			defer wg.Done()
			for k := 0; k < 20; k++ {
				s.expectPublish(true)
			}
		}(s)
	}
	wg.Wait()
}

// A subscriber that stops reading while a publisher floods QoS 0: the broker drops messages for it
// instead of buffering without limit, and keeps serving others.
func TestSlowSubscriberQoS0(t *testing.T) {
	b, addr := startBroker(t, Config{MaxClientBuffer: 64 << 10})
	slow := connect(t, addr, "slow", connectOpts{clean: true})
	slow.subscribe(1, "flood", 0)
	fast := connect(t, addr, "fast", connectOpts{clean: true})
	fast.subscribe(1, "ping", 0)
	pub := connect(t, addr, "pub", connectOpts{clean: true})
	payload := make([]byte, 4096)
	pkt := appendPublish(nil, version311, "flood", payload, 0, false, false, 0, nil, 0)
	for i := 0; i < 5000; i++ { // 20MB, far more than the socket buffers and the limit
		pub.send(pkt)
	}
	waitDropped := time.Now().Add(5 * time.Second)
	for b.Stats().MessagesDropped == 0 {
		if time.Now().After(waitDropped) {
			t.Fatalf("nothing dropped for the slow subscriber: %+v", b.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	pub.publish("ping", "still alive", 0, false, 0)
	if p := fast.expectPublish(true); string(p.payload) != "still alive" {
		t.Fatalf("%+v", p)
	}
	t.Logf("stats %+v", b.Stats())
}

func TestLocalSubscribe(t *testing.T) {
	b, addr := startBroker(t, Config{})
	got := make(chan Message, 8)
	cancel, err := b.Subscribe("cmd/+/set", func(m *Message) {
		got <- Message{Topic: m.Topic, Payload: append([]byte(nil), m.Payload...), ClientID: m.ClientID}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Subscribe("a/#/b", func(*Message) {}); err == nil {
		t.Fatal("invalid filter accepted")
	}
	pub := connect(t, addr, "dev", connectOpts{clean: true})
	pub.publish("cmd/7/set", "on", 1, false, 1)
	pub.publish("cmd/7/other", "x", 0, false, 0)
	if m := <-got; m.Topic != "cmd/7/set" || string(m.Payload) != "on" || m.ClientID != "dev" {
		t.Fatalf("%+v", m)
	}
	b.Publish("cmd/8/set", []byte("off"), 0, false) // from the application too
	if m := <-got; m.Topic != "cmd/8/set" || m.ClientID != "" {
		t.Fatalf("%+v", m)
	}
	cancel()
	cancel() // idempotent
	b.Publish("cmd/9/set", []byte("x"), 0, false)
	select {
	case m := <-got:
		t.Fatalf("after cancel: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
	if st := b.Stats(); st.Subscriptions != 0 {
		t.Fatalf("local subscriptions counted as client subscriptions: %+v", st)
	}
}
