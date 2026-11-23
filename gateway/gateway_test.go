//go:build linux
// +build linux

package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/y001j/uringnet"
	"github.com/y001j/uringnet/modbus"
	"github.com/y001j/uringnet/mqtt"
)

func opts() []uringnet.Option {
	o := []uringnet.Option{uringnet.WithRings(2)}
	if os.Getenv("URINGNET_BACKEND") == "epoll" {
		o = append(o, uringnet.WithBackend(uringnet.BackendEpoll))
	}
	return o
}

// rig is a broker, a Modbus slave and a client link to it.
type rig struct {
	broker *mqtt.Broker
	mem    *modbus.Memory
	link   *modbus.Link
	msgs   chan mqtt.Message
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{broker: mqtt.NewBroker(mqtt.Config{}), mem: modbus.NewMemory(8, 8, 32, 32), msgs: make(chan mqtt.Message, 1024)}
	t.Cleanup(r.broker.Close)
	srv, err := uringnet.Listen("tcp4", "127.0.0.1:0", modbus.NewServer(r.mem, modbus.TCP).Handler(), opts()...)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	c, err := modbus.NewClient(modbus.ClientConfig{}, opts()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	r.link = c.Dial(srv.Addr().String())
	// Everything the gateway publishes, seen as an MQTT subscriber would
	cancel, err := r.broker.Subscribe("#", func(m *mqtt.Message) {
		cp := *m
		cp.Payload = append([]byte(nil), m.Payload...)
		r.msgs <- cp
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	return r
}

// next returns the next message on topic.
func (r *rig) next(t *testing.T, topic string) mqtt.Message {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m := <-r.msgs:
			if m.Topic == topic {
				return m
			}
		case <-timeout:
			t.Fatalf("nothing on %s", topic)
		}
	}
}

func (r *rig) none(t *testing.T, topic string, d time.Duration) {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case m := <-r.msgs:
			if m.Topic == topic {
				t.Fatalf("unexpected message on %s: %s", topic, m.Payload)
			}
		case <-timeout:
			return
		}
	}
}

type valueMsg struct {
	Value any    `json:"value"`
	Error string `json:"error"`
	TS    int64  `json:"ts"`
}

func decode(t *testing.T, m mqtt.Message) valueMsg {
	t.Helper()
	var v valueMsg
	if err := json.Unmarshal(m.Payload, &v); err != nil {
		t.Fatalf("%s: %v", m.Payload, err)
	}
	return v
}

var points = []modbus.Point{
	{Name: "voltage", Table: modbus.InputRegister, Address: 0, Scale: 0.1},
	{Name: "setpoint", Address: 5, Type: modbus.Int16},
	{Name: "relay", Table: modbus.Coil, Address: 1, Type: modbus.Bool},
}

func TestGatewayValuesAndStatus(t *testing.T) {
	r := newRig(t)
	r.mem.SetInputRegisters(0, []uint16{2301})
	g, err := New(Config{Broker: r.broker, Prefix: "plant/a", Poller: modbus.PollerConfig{Interval: 100 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.Poller().Add(modbus.Device{Name: "meter1", Link: modbus.LinkOf(r.link), Points: points}); err != nil {
		t.Fatal(err)
	}
	if m := r.next(t, "plant/a/meter1/status"); string(m.Payload) != "online" || !m.Retain {
		t.Fatalf("status %q retain=%v", m.Payload, m.Retain)
	}
	v := decode(t, r.next(t, "plant/a/meter1/voltage"))
	if v.Value.(float64) != 230.1 || v.TS == 0 || v.Error != "" {
		t.Fatalf("%+v", v)
	}
	if v := decode(t, r.next(t, "plant/a/meter1/relay")); v.Value != false {
		t.Fatalf("%+v", v)
	}
	// Values follow the device
	r.mem.SetInputRegisters(0, []uint16{2250})
	for {
		if v := decode(t, r.next(t, "plant/a/meter1/voltage")); v.Value.(float64) == 225.0 {
			break
		}
	}
	// Writes are off by default: a command does nothing
	r.broker.Publish("plant/a/meter1/setpoint/set", []byte("42"), 1, false)
	r.none(t, "plant/a/meter1/setpoint/set/result", 300*time.Millisecond)
	if regs, _ := r.mem.HoldingRegisters(5, 1); regs[0] != 0 {
		t.Fatal("write performed with Writes off")
	}
}

func TestGatewayOfflineAndChanges(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	var link *modbus.Link // nil: the DTU is offline
	g, err := New(Config{Broker: r.broker, OnlyChanges: true, Poller: modbus.PollerConfig{Interval: 50 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.Poller().Add(modbus.Device{Name: "dtu1", Points: points[:1], Link: func() *modbus.Link {
		mu.Lock()
		defer mu.Unlock()
		return link
	}})
	if m := r.next(t, "devices/dtu1/status"); string(m.Payload) != "offline" {
		t.Fatalf("%q", m.Payload)
	}
	if v := decode(t, r.next(t, "devices/dtu1/voltage")); !strings.Contains(v.Error, "offline") {
		t.Fatalf("%+v", v)
	}
	// OnlyChanges: the same error is not published again
	r.none(t, "devices/dtu1/voltage", 300*time.Millisecond)

	// The DTU comes online
	mu.Lock()
	link = r.link
	mu.Unlock()
	if m := r.next(t, "devices/dtu1/status"); string(m.Payload) != "online" {
		t.Fatalf("%q", m.Payload)
	}
	r.next(t, "devices/dtu1/voltage")
	r.none(t, "devices/dtu1/voltage", 300*time.Millisecond) // unchanged value
	r.mem.SetInputRegisters(0, []uint16{7})
	if v := decode(t, r.next(t, "devices/dtu1/voltage")); v.Value.(float64) != 0.7 {
		t.Fatalf("%+v", v)
	}
}

func TestGatewayPerDevice(t *testing.T) {
	r := newRig(t)
	r.mem.SetHoldingRegisters(5, []uint16{0xFFFF})
	g, err := New(Config{Broker: r.broker, Layout: PerDevice, Retain: true, Poller: modbus.PollerConfig{Interval: 100 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.Poller().Add(modbus.Device{Name: "m", Link: modbus.LinkOf(r.link), Points: append(points, modbus.Point{Name: "bad", Address: 100})})
	m := r.next(t, "devices/m")
	var msg struct {
		TS     int64             `json:"ts"`
		Values map[string]any    `json:"values"`
		Errors map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(m.Payload, &msg); err != nil {
		t.Fatal(err)
	}
	if !m.Retain || msg.Values["setpoint"] != -1.0 || msg.Values["relay"] != false || !strings.Contains(msg.Errors["bad"], "illegal data address") {
		t.Fatalf("%s", m.Payload)
	}
}

func TestGatewayWrites(t *testing.T) {
	r := newRig(t)
	g, err := New(Config{Broker: r.broker, Writes: true, Poller: modbus.PollerConfig{Interval: time.Hour},
		AuthorizeWrite: func(clientID, device, point string) bool { return point != "relay" || clientID == "" }})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.Poller().Add(modbus.Device{Name: "plc", Link: modbus.LinkOf(r.link), Points: points})
	type result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	write := func(point, payload string) result {
		t.Helper()
		r.broker.Publish("devices/plc/"+point+"/set", []byte(payload), 1, false)
		var res result
		json.Unmarshal(r.next(t, "devices/plc/"+point+"/set/result").Payload, &res)
		return res
	}
	for _, payload := range []string{"-12", `{"value": -12}`, `"-12"`} {
		if res := write("setpoint", payload); !res.OK {
			t.Fatalf("%s: %+v", payload, res)
		}
		if regs, _ := r.mem.HoldingRegisters(5, 1); regs[0] != 0xFFF4 {
			t.Fatalf("%s: register % x", payload, regs)
		}
		r.mem.SetHoldingRegisters(5, []uint16{0})
	}
	if res := write("relay", "on"); !res.OK {
		t.Fatalf("%+v", res)
	}
	if coils, _ := r.mem.Coils(1, 1); !coils[0] {
		t.Fatal("relay not switched")
	}
	for point, payload := range map[string]string{"voltage": "1", "setpoint": "99999", "nope": "1"} {
		if res := write(point, payload); res.OK || res.Error == "" {
			t.Errorf("%s=%s: %+v", point, payload, res)
		}
	}

	// MQTT 5 request/response: the result goes to the response topic with the correlation data
	r.broker.PublishMessage(&mqtt.Message{Topic: "devices/plc/setpoint/set", Payload: []byte("7"), QoS: 1,
		Properties: &mqtt.Properties{ResponseTopic: "app/replies", CorrelationData: []byte("req-42")}})
	m := r.next(t, "app/replies")
	if m.Properties == nil || string(m.Properties.CorrelationData) != "req-42" || !strings.Contains(string(m.Payload), `"ok":true`) {
		t.Fatalf("%+v %s", m.Properties, m.Payload)
	}

	// AuthorizeWrite sees the client ID: a device client may not switch the relay
	srv, err := uringnet.Listen("tcp4", "127.0.0.1:0", r.broker.Handler(), opts()...)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()
	if code := mqttPublish(t, srv.Addr().String(), "devices/plc/relay/set", "off"); code != 0 {
		t.Fatalf("CONNACK %d", code)
	}
	var res result
	json.Unmarshal(r.next(t, "devices/plc/relay/set/result").Payload, &res)
	if res.OK || !strings.Contains(res.Error, "not authorized") {
		t.Fatalf("%+v", res)
	}
	if coils, _ := r.mem.Coils(1, 1); !coils[0] {
		t.Fatal("unauthorized write performed")
	}
}

// mqttPublish connects as client "field-dev" (MQTT 3.1.1), publishes a QoS 0 message and returns
// the CONNACK return code.
func mqttPublish(t *testing.T, addr, topic, payload string) byte {
	t.Helper()
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	str := func(s string) []byte { return append([]byte{byte(len(s) >> 8), byte(len(s))}, s...) }
	body := append(str("MQTT"), 4, 0x02, 0, 30)
	body = append(body, str("field-dev")...)
	c.Write(append([]byte{0x10, byte(len(body))}, body...))
	rd := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	ack := make([]byte, 4)
	if _, err := io.ReadFull(rd, ack); err != nil {
		t.Fatal(err)
	}
	pub := append(str(topic), payload...)
	c.Write(append([]byte{0x30, byte(len(pub))}, pub...))
	c.Write([]byte{0xE0, 0}) // DISCONNECT
	return ack[3]
}
