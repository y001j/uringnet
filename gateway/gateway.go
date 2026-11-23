//go:build linux
// +build linux

// Package gateway bridges Modbus devices and MQTT in one process: a modbus.Poller reads the
// devices' points and the values are published to an mqtt.Broker; optionally, MQTT messages write
// points.
//
// Topics, under Config.Prefix (default "devices"):
//
//	<prefix>/<device>/<point>             a point's value: {"value": 230.1, "ts": 1700000000000}
//	                                      or {"error": "modbus: request timed out", "ts": ...}
//	<prefix>/<device>                     with Layout PerDevice instead: {"ts": ..., "values": {...}, "errors": {...}}
//	<prefix>/<device>/status              "online" or "offline", retained
//	<prefix>/<device>/<point>/set         (Writes) a value to write: 42, 21.5, true, "text" or {"value": 42}
//	<prefix>/<device>/<point>/set/result  (Writes) {"ok": true, "ts": ...} or {"ok": false, "error": "..."},
//	                                      or to the MQTT 5 response topic of the command, with its correlation data
//
// Timestamps are Unix milliseconds.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/y001j/uringnet/modbus"
	"github.com/y001j/uringnet/mqtt"
)

// Layout is how values are published.
type Layout int

const (
	// PerPoint publishes one message per point to <prefix>/<device>/<point> (the default).
	PerPoint Layout = iota
	// PerDevice publishes one message per poll to <prefix>/<device> with all its values.
	PerDevice
)

// Config configures a Gateway.
type Config struct {
	// Broker receives the values and, with Writes, the write commands.
	Broker *mqtt.Broker
	// Poller configures the gateway's poller. Its OnValues, if set, is called too, after publishing.
	Poller modbus.PollerConfig
	// Prefix is the first topic level(s) (default "devices").
	Prefix string
	Layout Layout
	// QoS and Retain apply to value messages. Retained values give new subscribers the latest
	// value at once.
	QoS    byte
	Retain bool
	// OnlyChanges publishes a value only when it (or its error) differs from the last one published.
	OnlyChanges bool
	// Writes enables write commands on <prefix>/<device>/<point>/set. It is off by default:
	// anyone allowed to publish there controls the devices. Restrict who may with
	// mqtt.Config.AuthorizePublish or AuthorizeWrite.
	Writes bool
	// AuthorizeWrite, if set, accepts or rejects a write command; clientID is "" for messages
	// published by the application.
	AuthorizeWrite func(clientID, device, point string) bool
	// WriteTimeout bounds a write (default 5s).
	WriteTimeout time.Duration
}

// Gateway publishes Modbus values to MQTT and writes points from MQTT commands.
type Gateway struct {
	cfg    Config
	poller *modbus.Poller
	unsub  func()

	mu     sync.Mutex
	online map[string]bool
	last   map[string]string // OnlyChanges: topic -> last published value or error
}

// New creates a gateway and starts its poller; add devices with g.Poller().Add.
func New(cfg Config) (*Gateway, error) {
	if cfg.Broker == nil {
		return nil, errors.New("gateway: no broker")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "devices"
	}
	cfg.Prefix = strings.TrimSuffix(cfg.Prefix, "/")
	if strings.ContainsAny(cfg.Prefix, "+#") {
		return nil, fmt.Errorf("gateway: invalid prefix %q", cfg.Prefix)
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 5 * time.Second
	}
	if cfg.QoS > 2 {
		return nil, fmt.Errorf("gateway: invalid QoS %d", cfg.QoS)
	}
	g := &Gateway{cfg: cfg, online: make(map[string]bool), last: make(map[string]string)}
	user := cfg.Poller.OnValues
	pc := cfg.Poller
	pc.OnValues = func(device string, values []modbus.Value) {
		g.publish(device, values)
		if user != nil {
			user(device, values)
		}
	}
	g.poller = modbus.NewPoller(pc)
	if cfg.Writes {
		unsub, err := cfg.Broker.Subscribe(cfg.Prefix+"/+/+/set", g.command)
		if err != nil {
			g.poller.Close()
			return nil, err
		}
		g.unsub = unsub
	}
	return g, nil
}

// Poller returns the gateway's poller, to add and remove devices.
func (g *Gateway) Poller() *modbus.Poller { return g.poller }

// Close stops polling and write commands.
func (g *Gateway) Close() {
	if g.unsub != nil {
		g.unsub()
	}
	g.poller.Close()
}

func millis(t time.Time) int64 { return t.UnixNano() / 1e6 }

// jsonValue makes a value JSON-encodable (JSON has no NaN or infinities).
func jsonValue(v any) any {
	if f, ok := v.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
		return fmt.Sprint(f)
	}
	return v
}

func (g *Gateway) pub(topic string, payload []byte, qos byte, retain bool) {
	g.cfg.Broker.Publish(topic, payload, qos, retain)
}

// changed reports whether a topic's value differs from the last one published, and records it.
func (g *Gateway) changed(topic, sig string) bool {
	if !g.cfg.OnlyChanges {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if old, ok := g.last[topic]; ok && old == sig {
		return false
	}
	g.last[topic] = sig
	return true
}

func signature(v modbus.Value) string {
	if v.Err != nil {
		return "error:" + v.Err.Error()
	}
	return fmt.Sprintf("%T:%v", v.Value, v.Value)
}

// publish sends one poll's values and the device's status.
func (g *Gateway) publish(device string, values []modbus.Value) {
	base := g.cfg.Prefix + "/" + device
	up := false
	for _, v := range values {
		if v.Err == nil {
			up = true
			break
		}
	}
	g.mu.Lock()
	was, known := g.online[device]
	g.online[device] = up
	g.mu.Unlock()
	if !known || was != up {
		status := "offline"
		if up {
			status = "online"
		}
		g.pub(base+"/status", []byte(status), 1, true)
	}

	if g.cfg.Layout == PerDevice {
		msg := struct {
			TS     int64             `json:"ts"`
			Values map[string]any    `json:"values,omitempty"`
			Errors map[string]string `json:"errors,omitempty"`
		}{TS: millis(time.Now())}
		var sig strings.Builder
		for _, v := range values {
			sig.WriteString(v.Point.Name + "=" + signature(v) + ";")
			if v.Err != nil {
				if msg.Errors == nil {
					msg.Errors = make(map[string]string)
				}
				msg.Errors[v.Point.Name] = v.Err.Error()
				continue
			}
			if msg.Values == nil {
				msg.Values = make(map[string]any)
			}
			msg.Values[v.Point.Name] = jsonValue(v.Value)
		}
		// Devices with several interval groups publish each group's values; key changes by group
		if g.changed(base+"#"+values[0].Point.Name, sig.String()) {
			b, _ := json.Marshal(msg)
			g.pub(base, b, g.cfg.QoS, g.cfg.Retain)
		}
		return
	}
	for _, v := range values {
		topic := base + "/" + v.Point.Name
		if !g.changed(topic, signature(v)) {
			continue
		}
		var b []byte
		if v.Err != nil {
			b, _ = json.Marshal(struct {
				Error string `json:"error"`
				TS    int64  `json:"ts"`
			}{v.Err.Error(), millis(v.Time)})
		} else {
			b, _ = json.Marshal(struct {
				Value any   `json:"value"`
				TS    int64 `json:"ts"`
			}{jsonValue(v.Value), millis(v.Time)})
		}
		g.pub(topic, b, g.cfg.QoS, g.cfg.Retain)
	}
}

// parseCommand decodes a write command's payload: JSON (number, bool, string, or {"value": x}),
// or plain text.
func parseCommand(payload []byte) (any, error) {
	p := bytes.TrimSpace(payload)
	if len(p) == 0 {
		return nil, errors.New("empty command")
	}
	d := json.NewDecoder(bytes.NewReader(p))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil || d.More() {
		return string(p), nil // plain text: "21.5", "on", "hello"
	}
	if obj, ok := v.(map[string]any); ok {
		x, ok := obj["value"]
		if !ok {
			return nil, errors.New(`command object without "value"`)
		}
		v = x
	}
	switch v.(type) {
	case json.Number, bool, string:
		return v, nil
	}
	return nil, fmt.Errorf("unsupported command value %T", v)
}

// command handles a message on <prefix>/<device>/<point>/set (on the publisher's event loop).
func (g *Gateway) command(m *mqtt.Message) {
	rest := strings.TrimSuffix(strings.TrimPrefix(m.Topic, g.cfg.Prefix+"/"), "/set")
	device, point, ok := strings.Cut(rest, "/")
	if !ok {
		return
	}
	// Where the result goes: the MQTT 5 response topic, or <topic>/result
	replyTo := m.Topic + "/result"
	var corr []byte
	if mp := m.Properties; mp != nil && mp.ResponseTopic != "" {
		replyTo = mp.ResponseTopic
		corr = append([]byte(nil), mp.CorrelationData...)
	}
	reply := func(err error) {
		res := struct {
			OK    bool   `json:"ok"`
			Error string `json:"error,omitempty"`
			TS    int64  `json:"ts"`
		}{OK: err == nil, TS: millis(time.Now())}
		if err != nil {
			res.Error = err.Error()
		}
		b, _ := json.Marshal(res)
		msg := &mqtt.Message{Topic: replyTo, Payload: b, QoS: 1}
		if corr != nil {
			msg.Properties = &mqtt.Properties{CorrelationData: corr}
		}
		g.cfg.Broker.PublishMessage(msg)
	}
	if g.cfg.AuthorizeWrite != nil && !g.cfg.AuthorizeWrite(m.ClientID, device, point) {
		reply(errors.New("gateway: write not authorized"))
		return
	}
	v, err := parseCommand(m.Payload)
	if err != nil {
		reply(fmt.Errorf("gateway: %w", err))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.cfg.WriteTimeout)
	g.poller.WriteAsync(ctx, device, point, v, func(err error) {
		cancel()
		reply(err)
	})
}
