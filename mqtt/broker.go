//go:build linux
// +build linux

package mqtt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/y001j/uringnet"
)

// Config configures a Broker. All callbacks may run concurrently on different event loops and
// must not block.
type Config struct {
	// Authenticate accepts or rejects a connecting client; nil accepts everyone.
	Authenticate func(clientID, username string, password []byte) bool
	// AuthorizePublish decides whether a client may publish to topic; nil allows all. Denied
	// messages are acknowledged (MQTT 3.1.1 cannot report the denial) and dropped.
	AuthorizePublish func(clientID, topic string) bool
	// AuthorizeSubscribe decides whether a client may subscribe to filter; nil allows all.
	AuthorizeSubscribe func(clientID, filter string) bool
	// OnPublish receives every message published by a client (after authorization), on the
	// publisher's event loop. m.Payload is only valid during the call; copy it to keep it.
	OnPublish func(m *Message)
	// OnConnect and OnDisconnect report sessions coming and going. err is nil after a DISCONNECT.
	OnConnect    func(clientID string)
	OnDisconnect func(clientID string, err error)

	// MaxPacketSize limits incoming packets (default 1MB); larger ones close the connection.
	MaxPacketSize int
	// MaxInflight limits unacknowledged QoS 1 messages per session (default 64); more are queued.
	MaxInflight int
	// MaxQueued limits messages queued per session while it is offline or its inflight window is
	// full (default 1000); further messages are dropped and counted.
	MaxQueued int
	// MaxClientBuffer drops QoS 0 messages for a client whose unsent data exceeds it (default 1MB),
	// so a subscriber that reads slowly cannot make the broker buffer without limit. QoS 1 is bounded
	// by MaxInflight and MaxQueued instead.
	MaxClientBuffer int
	// ConnectTimeout closes connections that send no CONNECT within it (default 10s).
	ConnectTimeout time.Duration
	// Logger receives protocol errors at debug level; nil logs nothing.
	Logger *slog.Logger
}

// ErrInvalidTopic is returned by Publish for an empty topic or one containing wildcards.
var ErrInvalidTopic = errors.New("mqtt: invalid topic name")

// errProtocol closes connections that break the protocol.
var errProtocol = errors.New("mqtt: protocol violation")

// Broker is an MQTT 3.1.1 broker. Serve it with uringnet.Listen("tcp", ":1883", b.Handler(), ...);
// one Broker may serve several listeners (e.g. plain and TLS).
//
// Subscriptions are granted at most QoS 1; incoming QoS 2 publishes are fully supported (delivered
// once). Sessions, queues and retained messages are kept in memory only.
type Broker struct {
	cfg  Config
	subs *subscriptions[*session]
	log  *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
	clients  map[*client]struct{}
	autoID   uint64

	retainedMu sync.RWMutex
	retained   map[string]*Message

	received, sent, dropped atomic.Uint64

	stop chan struct{}
	once sync.Once
}

// Stats are a broker's counters.
type Stats struct {
	Clients          int    // connected clients
	Sessions         int    // sessions, including offline persistent ones
	Subscriptions    int    // subscriptions over all sessions
	Retained         int    // retained messages
	MessagesReceived uint64 // PUBLISH packets received from clients
	MessagesSent     uint64 // PUBLISH packets sent to clients
	MessagesDropped  uint64 // messages dropped: a session's queue was full, or a slow client's buffer (QoS 0)
}

// NewBroker creates a broker.
func NewBroker(cfg Config) *Broker {
	if cfg.MaxPacketSize <= 0 {
		cfg.MaxPacketSize = 1 << 20
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = 64
	}
	if cfg.MaxQueued <= 0 {
		cfg.MaxQueued = 1000
	}
	if cfg.MaxClientBuffer <= 0 {
		cfg.MaxClientBuffer = 1 << 20
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	b := &Broker{cfg: cfg, subs: newSubscriptions[*session](), sessions: make(map[string]*session),
		clients: make(map[*client]struct{}), retained: make(map[string]*Message), stop: make(chan struct{})}
	b.log = cfg.Logger
	if b.log == nil {
		b.log = slog.New(discard{})
	}
	go b.watch()
	return b
}

// Handler returns the uringnet handler serving MQTT for this broker.
func (b *Broker) Handler() uringnet.Handler {
	return uringnet.Framed(framer{max: b.cfg.MaxPacketSize}, &handler{b: b})
}

// Close stops the broker's background checks (keepalive and connect timeouts). Close the
// uringnet servers to close the connections.
func (b *Broker) Close() { b.once.Do(func() { close(b.stop) }) }

// Publish sends a message to the subscribers of topic, as if a client had published it (it is
// retained if retain is set). It is safe to call from any goroutine.
func (b *Broker) Publish(topic string, payload []byte, qos byte, retain bool) error {
	if !validTopicName(topic) {
		return ErrInvalidTopic
	}
	if qos > 2 {
		return fmt.Errorf("mqtt: invalid QoS %d", qos)
	}
	b.route(nil, &Message{Topic: topic, Payload: payload, QoS: qos, Retain: retain})
	return nil
}

// Stats returns the broker's counters.
func (b *Broker) Stats() Stats {
	b.mu.Lock()
	clients := 0
	for c := range b.clients {
		if c.connected.Load() {
			clients++
		}
	}
	sessions := len(b.sessions)
	b.mu.Unlock()
	b.retainedMu.RLock()
	retained := len(b.retained)
	b.retainedMu.RUnlock()
	return Stats{Clients: clients, Sessions: sessions, Subscriptions: b.subs.count(), Retained: retained,
		MessagesReceived: b.received.Load(), MessagesSent: b.sent.Load(), MessagesDropped: b.dropped.Load()}
}

// session is the state of a client ID, which may outlive its connection (clean session = 0).
type session struct {
	b  *Broker
	id string

	mu       sync.Mutex
	clean    bool
	client   *client         // current connection, nil while offline
	filters  map[string]byte // filter -> granted QoS
	nextID   uint16
	inflight map[uint16]*Message // QoS 1 sent, waiting for PUBACK
	order    []uint16            // inflight ids in send order, for resending
	queue    []*Message          // QoS 1 waiting for a connection or an inflight slot
	inQoS2   map[uint16]bool     // incoming QoS 2 ids waiting for PUBREL
}

func newSession(b *Broker, id string, clean bool) *session {
	return &session{b: b, id: id, clean: clean, filters: make(map[string]byte),
		inflight: make(map[uint16]*Message), inQoS2: make(map[uint16]bool)}
}

// client is one network connection.
type client struct {
	conn      *uringnet.Conn
	s         *session // set once connected; only read by the connection's loop
	connected atomic.Bool
	created   int64
	lastSeen  atomic.Int64
	keepAlive time.Duration
	will      *Message
	clientID  string
	graceful  bool // DISCONNECT received
}

var clockBase = time.Now()

func now() int64 { return int64(time.Since(clockBase)) }

// send writes pkt to "to" on to's event loop: directly when the caller already runs there (from is
// a connection of that loop), otherwise posted to it. pkt must not change afterwards (QoS 0 packets
// are shared by all subscribers). With droppable set (QoS 0) the packet is dropped if the client
// already has more than MaxClientBuffer unsent.
func (b *Broker) send(from, to *uringnet.Conn, pkt []byte, droppable bool) {
	write := func() {
		if droppable && to.Buffered() > b.cfg.MaxClientBuffer {
			b.dropped.Add(1)
			return
		}
		if _, err := to.Write(pkt); err == nil {
			b.sent.Add(1)
		}
	}
	if from != nil && from.SameLoop(to) {
		write()
		return
	}
	to.Post(write)
}

// route delivers a message to retained storage, the application and the subscribers. from is the
// connection whose loop runs the call, nil from other goroutines.
func (b *Broker) route(from *uringnet.Conn, m *Message) {
	if m.Retain {
		b.retainedMu.Lock()
		if len(m.Payload) == 0 {
			delete(b.retained, m.Topic) // an empty retained message clears the topic
		} else {
			b.retained[m.Topic] = &Message{Topic: m.Topic, Payload: append([]byte(nil), m.Payload...), QoS: m.QoS, Retain: true}
		}
		b.retainedMu.Unlock()
	}
	if m.ClientID != "" && b.cfg.OnPublish != nil {
		b.cfg.OnPublish(m)
	}
	var qos0 []byte // encoded once for every QoS 0 subscriber
	b.subs.match(m.Topic, func(s *session, granted byte) {
		qos := m.QoS
		if granted < qos {
			qos = granted
		}
		if qos == 0 {
			if qos0 == nil {
				qos0 = appendPublish(nil, m.Topic, m.Payload, 0, false, false, 0)
			}
			s.deliver0(from, qos0)
			return
		}
		s.deliver1(from, m, false)
	})
}

// deliver0 sends an encoded QoS 0 PUBLISH if the session is connected (QoS 0 is not queued).
func (s *session) deliver0(from *uringnet.Conn, pkt []byte) {
	s.mu.Lock()
	cl := s.client
	s.mu.Unlock()
	if cl != nil {
		s.b.send(from, cl.conn, pkt, true)
	}
}

// deliver1 sends m with QoS 1, or queues it while the session is offline or its window is full.
func (s *session) deliver1(from *uringnet.Conn, m *Message, retain bool) {
	msg := &Message{Topic: m.Topic, Payload: append([]byte(nil), m.Payload...), QoS: 1, Retain: retain}
	s.mu.Lock()
	cl := s.client
	if cl == nil || len(s.inflight) >= s.b.cfg.MaxInflight {
		if len(s.queue) >= s.b.cfg.MaxQueued {
			s.mu.Unlock()
			s.b.dropped.Add(1)
			return
		}
		s.queue = append(s.queue, msg)
		s.mu.Unlock()
		return
	}
	pkt := s.startLocked(msg)
	s.mu.Unlock()
	s.b.send(from, cl.conn, pkt, false)
}

// startLocked assigns a packet id to msg, records it as inflight and encodes it.
func (s *session) startLocked(msg *Message) []byte {
	for {
		s.nextID++
		if s.nextID != 0 && s.inflight[s.nextID] == nil {
			break
		}
	}
	id := s.nextID
	s.inflight[id] = msg
	s.order = append(s.order, id)
	return appendPublish(nil, msg.Topic, msg.Payload, 1, msg.Retain, false, id)
}

// fillLocked starts queued messages while the window has room and returns the packets to send.
func (s *session) fillLocked(out []byte) []byte {
	for len(s.queue) > 0 && len(s.inflight) < s.b.cfg.MaxInflight {
		msg := s.queue[0]
		s.queue[0] = nil
		s.queue = s.queue[1:]
		out = append(out, s.startLocked(msg)...)
	}
	if len(s.queue) == 0 {
		s.queue = nil
	}
	return out
}

// handler serves MQTT connections for a Broker.
type handler struct {
	uringnet.BuiltinHandler
	b *Broker
}

func (h *handler) OnOpen(c *uringnet.Conn) uringnet.Action {
	t := now()
	cl := &client{conn: c, created: t}
	cl.lastSeen.Store(t)
	c.SetContext(cl)
	h.b.mu.Lock()
	h.b.clients[cl] = struct{}{}
	h.b.mu.Unlock()
	return uringnet.None
}

func (h *handler) OnMessage(c *uringnet.Conn, pkt []byte) uringnet.Action {
	cl := c.Context().(*client)
	cl.lastSeen.Store(now())
	typ, flags, body := header(pkt)
	if !cl.connected.Load() {
		if typ != typeConnect {
			return h.fail(cl, errProtocol, "first packet is not CONNECT")
		}
		return h.connect(cl, flags, body)
	}
	b, s := h.b, cl.s
	switch typ {
	case typePublish:
		return h.publish(cl, flags, body)
	case typePuback:
		id, err := parseID(body)
		if err != nil {
			return h.fail(cl, err, "PUBACK")
		}
		s.mu.Lock()
		if s.inflight[id] != nil {
			delete(s.inflight, id)
			for i, x := range s.order {
				if x == id {
					s.order = append(s.order[:i], s.order[i+1:]...)
					break
				}
			}
		}
		out := s.fillLocked(nil)
		s.mu.Unlock()
		if len(out) > 0 {
			b.sent.Add(1)
			c.Write(out)
		}
	case typePubrel:
		if flags != 0x02 {
			return h.fail(cl, errMalformed, "PUBREL flags")
		}
		id, err := parseID(body)
		if err != nil {
			return h.fail(cl, err, "PUBREL")
		}
		s.mu.Lock()
		delete(s.inQoS2, id)
		s.mu.Unlock()
		c.Write(appendAck(nil, typePubcomp, id))
	case typeSubscribe:
		return h.subscribe(cl, flags, body)
	case typeUnsubscribe:
		id, filters, err := parseUnsubscribe(flags, body)
		if err != nil {
			return h.fail(cl, err, "UNSUBSCRIBE")
		}
		for _, f := range filters {
			b.subs.remove(f, s)
			s.mu.Lock()
			delete(s.filters, f)
			s.mu.Unlock()
		}
		c.Write(appendAck(nil, typeUnsuback, id))
	case typePingreq:
		c.Write([]byte{typePingresp << 4, 0})
	case typeDisconnect:
		cl.graceful = true
		cl.will = nil // a clean disconnect discards the will
		return uringnet.Close
	case typePubrec, typePubcomp:
		// Only for outgoing QoS 2, which is never granted
	default:
		return h.fail(cl, errProtocol, fmt.Sprintf("unexpected packet type %d", typ))
	}
	return uringnet.None
}

func (h *handler) fail(cl *client, err error, what string) uringnet.Action {
	h.b.log.Debug("mqtt: closing connection", "client", cl.clientID, "reason", what, "err", err)
	return uringnet.Close
}

func (h *handler) connect(cl *client, flags byte, body []byte) uringnet.Action {
	b, c := h.b, cl.conn
	p, err := parseConnect(flags, body)
	if errors.Is(err, errBadProtocol) {
		if p != nil && p.protocolLevel == 5 {
			// An MQTT 5 client parses a v5 CONNACK: reason code 0x84 (unsupported protocol version), no properties
			c.Write([]byte{typeConnack << 4, 3, 0, 0x84, 0})
		} else {
			c.Write(appendConnack(nil, false, connBadProtocol))
		}
		return h.fail(cl, err, "CONNECT")
	}
	if err != nil {
		return h.fail(cl, err, "CONNECT")
	}
	id := p.clientID
	if id == "" {
		if !p.cleanSession {
			c.Write(appendConnack(nil, false, connIdentifierInvalid))
			return h.fail(cl, errProtocol, "empty client ID with a persistent session")
		}
		b.mu.Lock()
		b.autoID++
		id = "auto-" + strconv.FormatUint(b.autoID, 10)
		b.mu.Unlock()
	}
	if b.cfg.Authenticate != nil && !b.cfg.Authenticate(id, p.username, p.password) {
		c.Write(appendConnack(nil, false, connBadCredentials))
		return h.fail(cl, errProtocol, "authentication failed")
	}
	if p.will != nil && !validTopicName(p.will.Topic) {
		return h.fail(cl, errProtocol, "invalid will topic")
	}

	// Take over an existing session with this client ID
	b.mu.Lock()
	s := b.sessions[id]
	present := false
	if s != nil {
		s.mu.Lock()
		old := s.client
		s.client = nil
		keep := !p.cleanSession && !s.clean
		s.mu.Unlock()
		if old != nil {
			old.conn.AsyncClose() // the old connection's OnClose sees it no longer owns the session
		}
		if keep {
			present = true
		} else {
			b.dropSessionLocked(s)
			s = nil
		}
	}
	if s == nil {
		s = newSession(b, id, p.cleanSession)
		b.sessions[id] = s
	}
	s.mu.Lock()
	s.client = cl
	s.clean = p.cleanSession
	// A resumed session resends its unacknowledged messages (DUP set), then queued ones
	var out []byte
	for _, mid := range s.order {
		m := s.inflight[mid]
		out = appendPublish(out, m.Topic, m.Payload, 1, m.Retain, true, mid)
	}
	out = s.fillLocked(out)
	s.mu.Unlock()
	b.mu.Unlock()

	cl.s, cl.clientID, cl.will = s, id, p.will
	cl.keepAlive = time.Duration(p.keepAlive) * time.Second
	cl.connected.Store(true)
	c.Write(appendConnack(nil, present, connAccepted))
	if len(out) > 0 {
		b.sent.Add(1)
		c.Write(out)
	}
	if b.cfg.OnConnect != nil {
		b.cfg.OnConnect(id)
	}
	return uringnet.None
}

func (h *handler) publish(cl *client, flags byte, body []byte) uringnet.Action {
	b, c, s := h.b, cl.conn, cl.s
	p, err := parsePublish(flags, body)
	if err != nil {
		return h.fail(cl, err, "PUBLISH")
	}
	if !validTopicName(p.topic) {
		return h.fail(cl, errProtocol, "PUBLISH to an invalid topic")
	}
	b.received.Add(1)
	allowed := b.cfg.AuthorizePublish == nil || b.cfg.AuthorizePublish(cl.clientID, p.topic)
	m := &Message{Topic: p.topic, Payload: p.payload, QoS: p.qos, Retain: p.retain, ClientID: cl.clientID}
	switch p.qos {
	case 0:
		if allowed {
			b.route(c, m)
		}
	case 1:
		if allowed {
			b.route(c, m)
		}
		c.Write(appendAck(nil, typePuback, p.id))
	case 2:
		s.mu.Lock()
		dup := s.inQoS2[p.id]
		s.inQoS2[p.id] = true
		s.mu.Unlock()
		if allowed && !dup { // deliver once: a resent PUBLISH before PUBREL is not routed again
			b.route(c, m)
		}
		c.Write(appendAck(nil, typePubrec, p.id))
	}
	return uringnet.None
}

func (h *handler) subscribe(cl *client, flags byte, body []byte) uringnet.Action {
	b, c, s := h.b, cl.conn, cl.s
	id, subs, err := parseSubscribe(flags, body)
	if err != nil {
		return h.fail(cl, err, "SUBSCRIBE")
	}
	codes := make([]byte, len(subs))
	for i, sub := range subs {
		if !validFilter(sub.filter) || (b.cfg.AuthorizeSubscribe != nil && !b.cfg.AuthorizeSubscribe(cl.clientID, sub.filter)) {
			codes[i] = 0x80
			continue
		}
		granted := sub.qos
		if granted > 1 {
			granted = 1
		}
		codes[i] = granted
		b.subs.add(sub.filter, s, granted)
		s.mu.Lock()
		s.filters[sub.filter] = granted
		s.mu.Unlock()
	}
	c.Write(appendSuback(nil, id, codes))
	// Retained messages matching the new subscriptions follow the SUBACK
	for i, sub := range subs {
		if codes[i] == 0x80 {
			continue
		}
		for _, m := range b.retainedMatching(sub.filter) {
			qos := m.QoS
			if codes[i] < qos {
				qos = codes[i]
			}
			if qos == 0 {
				b.send(c, c, appendPublish(nil, m.Topic, m.Payload, 0, true, false, 0), false)
			} else {
				s.deliver1(c, m, true)
			}
		}
	}
	return uringnet.None
}

func (b *Broker) retainedMatching(filter string) []*Message {
	b.retainedMu.RLock()
	defer b.retainedMu.RUnlock()
	var out []*Message
	for topic, m := range b.retained {
		if matchFilter(filter, topic) {
			out = append(out, m)
		}
	}
	return out
}

func (h *handler) OnClose(c *uringnet.Conn, err error) {
	b := h.b
	cl := c.Context().(*client)
	b.mu.Lock()
	delete(b.clients, cl)
	b.mu.Unlock()
	if !cl.connected.Load() {
		return
	}
	if cl.will != nil && !cl.graceful {
		w := *cl.will
		w.ClientID = cl.clientID
		b.route(c, &w)
	}
	s := cl.s
	s.mu.Lock()
	owner := s.client == cl // false after a takeover by a newer connection
	if owner {
		s.client = nil
	}
	clean := s.clean
	s.mu.Unlock()
	if owner && clean {
		b.mu.Lock()
		if b.sessions[s.id] == s {
			b.dropSessionLocked(s)
		}
		b.mu.Unlock()
	}
	if b.cfg.OnDisconnect != nil {
		if cl.graceful {
			err = nil
		}
		b.cfg.OnDisconnect(cl.clientID, err)
	}
}

// dropSessionLocked removes a session and its subscriptions (b.mu held).
func (b *Broker) dropSessionLocked(s *session) {
	if b.sessions[s.id] == s {
		delete(b.sessions, s.id)
	}
	s.mu.Lock()
	filters := s.filters
	s.filters = make(map[string]byte)
	s.mu.Unlock()
	for f := range filters {
		b.subs.remove(f, s)
	}
}

// watch closes connections that sent no CONNECT in time, or nothing for 1.5 times their keepalive.
func (b *Broker) watch() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
		}
		n := now()
		b.mu.Lock()
		for cl := range b.clients {
			if !cl.connected.Load() {
				if time.Duration(n-cl.created) > b.cfg.ConnectTimeout {
					cl.conn.AsyncClose()
				}
				continue
			}
			if cl.keepAlive > 0 && time.Duration(n-cl.lastSeen.Load()) > cl.keepAlive*3/2 {
				cl.conn.AsyncClose()
			}
		}
		b.mu.Unlock()
	}
}

type discard struct{}

func (discard) Enabled(context.Context, slog.Level) bool  { return false }
func (discard) Handle(context.Context, slog.Record) error { return nil }
func (d discard) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discard) WithGroup(string) slog.Handler           { return d }
