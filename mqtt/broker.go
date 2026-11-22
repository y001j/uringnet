//go:build linux
// +build linux

package mqtt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
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
	// messages are dropped; MQTT 5 clients get reason code 0x87 (not authorized) in the
	// acknowledgement, MQTT 3.1.1 clients a normal acknowledgement (3.1.1 cannot report it).
	AuthorizePublish func(clientID, topic string) bool
	// AuthorizeSubscribe decides whether a client may subscribe to filter; nil allows all.
	AuthorizeSubscribe func(clientID, filter string) bool
	// OnPublish receives every message published by a client (after authorization), on the
	// publisher's event loop. m.Payload is only valid during the call; copy it to keep it.
	OnPublish func(m *Message)
	// OnConnect and OnDisconnect report connections coming and going. err is nil after a DISCONNECT.
	OnConnect    func(clientID string)
	OnDisconnect func(clientID string, err error)

	// SessionExpiry removes a session that has been offline for this long, with its subscriptions
	// and queued messages. It applies to MQTT 3.1.1 persistent sessions (clean session = 0) and caps
	// the session expiry MQTT 5 clients ask for. 0 means MQTT 3.1.1 persistent sessions never
	// expire, as the 3.1.1 specification says, which lets the sessions of devices that never come
	// back accumulate: set it in production.
	SessionExpiry time.Duration
	// MaxPacketSize limits incoming packets (default 1MB); larger ones close the connection. MQTT 5
	// clients are told the limit.
	MaxPacketSize int
	// MaxInflight limits unacknowledged QoS 1 messages per session (default 64), further limited by
	// an MQTT 5 client's receive maximum; more are queued.
	MaxInflight int
	// MaxQueued limits messages queued per session while it is offline or its inflight window is
	// full (default 1000); further messages are dropped and counted.
	MaxQueued int
	// MaxClientBuffer drops QoS 0 messages for a client whose unsent data exceeds it (default 1MB),
	// so a subscriber that reads slowly cannot make the broker buffer without limit. QoS 1 is bounded
	// by MaxInflight and MaxQueued instead.
	MaxClientBuffer int
	// TopicAliasMaximum is how many topic aliases an MQTT 5 client may use when publishing
	// (default 16; negative disables topic aliases).
	TopicAliasMaximum int
	// ConnectTimeout closes connections that send no CONNECT within it (default 10s).
	ConnectTimeout time.Duration
	// Logger receives protocol errors at debug level; nil logs nothing.
	Logger *slog.Logger
}

// ErrInvalidTopic is returned by Publish for an empty topic or one containing wildcards.
var ErrInvalidTopic = errors.New("mqtt: invalid topic name")

// Broker is an MQTT broker for protocol versions 5 and 3.1.1 (and 3.1); clients of all versions
// can talk to each other. Serve it with uringnet.Listen("tcp", ":1883", b.Handler(), ...); one
// Broker may serve several listeners.
//
// Subscriptions are granted at most QoS 1; incoming QoS 2 publishes are fully supported (delivered
// once). MQTT 5 shared subscriptions, subscription identifiers and enhanced authentication are not
// supported and are refused with the corresponding reason codes. Sessions, queues and retained
// messages are kept in memory only.
type Broker struct {
	cfg  Config
	subs *subscriptions[*session, subOpts]
	log  *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
	clients  map[*client]struct{}
	autoID   uint64

	retainedMu sync.RWMutex
	retained   map[string]*stored

	received, sent, dropped, expired, sessionsExpired atomic.Uint64

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
	MessagesDropped  uint64 // dropped: a session's queue was full, a slow client's buffer (QoS 0), or larger than the client accepts
	MessagesExpired  uint64 // discarded because their message expiry interval passed before delivery
	SessionsExpired  uint64 // sessions removed after their session expiry interval
}

// NewBroker creates a broker.
func NewBroker(cfg Config) *Broker {
	if cfg.MaxPacketSize <= 0 {
		cfg.MaxPacketSize = 1 << 20
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = 64
	}
	if cfg.MaxInflight > 65535 {
		cfg.MaxInflight = 65535
	}
	if cfg.MaxQueued <= 0 {
		cfg.MaxQueued = 1000
	}
	if cfg.MaxClientBuffer <= 0 {
		cfg.MaxClientBuffer = 1 << 20
	}
	switch {
	case cfg.TopicAliasMaximum == 0:
		cfg.TopicAliasMaximum = 16
	case cfg.TopicAliasMaximum < 0:
		cfg.TopicAliasMaximum = 0
	case cfg.TopicAliasMaximum > 65535:
		cfg.TopicAliasMaximum = 65535
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	b := &Broker{cfg: cfg, subs: newSubscriptions[*session, subOpts](), sessions: make(map[string]*session),
		clients: make(map[*client]struct{}), retained: make(map[string]*stored), stop: make(chan struct{})}
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

// Close stops the broker's background work (timeouts, session expiry). Close the uringnet servers
// to close the connections.
func (b *Broker) Close() { b.once.Do(func() { close(b.stop) }) }

// Publish sends a message to the subscribers of topic, as if a client had published it (it is
// retained if retain is set). It is safe to call from any goroutine.
func (b *Broker) Publish(topic string, payload []byte, qos byte, retain bool) error {
	return b.PublishMessage(&Message{Topic: topic, Payload: payload, QoS: qos, Retain: retain})
}

// PublishMessage is Publish with all message fields, including the MQTT 5 expiry and properties.
// m.ClientID is ignored.
func (b *Broker) PublishMessage(m *Message) error {
	if !validTopicName(m.Topic) {
		return ErrInvalidTopic
	}
	if m.QoS > 2 {
		return fmt.Errorf("mqtt: invalid QoS %d", m.QoS)
	}
	msg := *m
	msg.ClientID = ""
	b.route(nil, &msg, deadlineOf(m.Expiry))
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
		MessagesReceived: b.received.Load(), MessagesSent: b.sent.Load(), MessagesDropped: b.dropped.Load(),
		MessagesExpired: b.expired.Load(), SessionsExpired: b.sessionsExpired.Load()}
}

// subOpts are the options of one subscription.
type subOpts struct {
	qos               byte
	noLocal           bool
	retainAsPublished bool
}

// mergeOpts combines overlapping subscriptions of one session: the highest QoS, retain as published
// if any asks for it, and no local only if all of them ask for it.
func mergeOpts(a, b subOpts) subOpts {
	if b.qos > a.qos {
		a.qos = b.qos
	}
	a.retainAsPublished = a.retainAsPublished || b.retainAsPublished
	a.noLocal = a.noLocal && b.noLocal
	return a
}

// stored is a message kept by the broker (queued, inflight or retained) with its expiry deadline.
type stored struct {
	m        *Message
	deadline int64 // monotonic nanoseconds (see now), 0 for none
}

var clockBase = time.Now()

func now() int64 { return int64(time.Since(clockBase)) }

func deadlineOf(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return now() + int64(d)
}

// remaining returns the expiry interval left in whole seconds (rounded up) and whether the message
// has expired; 0 seconds means no expiry.
func remaining(deadline int64) (secs uint32, expired bool) {
	if deadline == 0 {
		return 0, false
	}
	left := deadline - now()
	if left <= 0 {
		return 0, true
	}
	return uint32((time.Duration(left) + time.Second - 1) / time.Second), false
}

// never is the expiry of sessions that do not expire.
const never time.Duration = -1

// session is the state of a client ID, which may outlive its connection.
type session struct {
	b  *Broker
	id string

	mu           sync.Mutex
	expiry       time.Duration // how long the session outlives its connection: 0 ends it with the connection, or never
	client       *client       // current connection, nil while offline
	offlineSince int64
	filters      map[string]subOpts
	nextID       uint16
	inflight     map[uint16]*stored // QoS 1 sent, waiting for PUBACK
	order        []uint16           // inflight ids in send order, for resending
	queue        []*stored          // QoS 1 waiting for a connection or an inflight slot
	inQoS2       map[uint16]bool    // incoming QoS 2 ids waiting for PUBREL
	window       int                // inflight limit: MaxInflight, or an MQTT 5 client's smaller receive maximum
	willTimer    *time.Timer        // pending delayed will (MQTT 5 will delay interval)
}

func newSession(b *Broker, id string) *session {
	return &session{b: b, id: id, filters: make(map[string]subOpts), inflight: make(map[uint16]*stored),
		inQoS2: make(map[uint16]bool), window: b.cfg.MaxInflight}
}

// client is one network connection.
type client struct {
	conn      *uringnet.Conn
	s         *session // set once connected
	connected atomic.Bool
	created   int64
	lastSeen  atomic.Int64
	keepAlive time.Duration
	version   byte
	clientID  string
	maxOut    int               // largest packet the client accepts (MQTT 5), 0 for no limit
	aliases   map[uint16]string // MQTT 5 topic aliases set by the client
	will      *Message
	willDelay time.Duration
	graceful  bool // DISCONNECT received (other than "disconnect with will message")
}

// encode builds a PUBLISH of st for this client; nil if the message has expired (a resend, dup,
// is never discarded).
func (cl *client) encode(st *stored, qos byte, retain, dup bool, id uint16) []byte {
	secs, expired := remaining(st.deadline)
	if expired && !dup {
		return nil
	}
	m := st.m
	return appendPublish(nil, cl.version, m.Topic, m.Payload, qos, retain, dup, id, m.Properties, secs)
}

// send writes pkt to "to" on to's event loop: directly when the caller already runs there (from is
// a connection of that loop), otherwise posted to it. pkt must not change afterwards (QoS 0 packets
// are shared by all subscribers). With droppable set (QoS 0) the packet is dropped if the client
// already has more than MaxClientBuffer unsent.
func (b *Broker) send(from *uringnet.Conn, to *client, pkt []byte, droppable bool) {
	if to.maxOut > 0 && len(pkt) > to.maxOut {
		b.dropped.Add(1) // larger than the client accepts (MQTT 5 maximum packet size)
		return
	}
	conn := to.conn
	write := func() {
		if droppable && conn.Buffered() > b.cfg.MaxClientBuffer {
			b.dropped.Add(1)
			return
		}
		if _, err := conn.Write(pkt); err == nil {
			b.sent.Add(1)
		}
	}
	if from != nil && from.SameLoop(conn) {
		write()
		return
	}
	conn.Post(write)
}

// qos0Cache encodes a QoS 0 PUBLISH once per protocol variant and retain flag, shared by all
// subscribers.
type qos0Cache struct {
	m    *Message
	secs uint32
	pkts [2][2][]byte // [MQTT 5][retain]
}

func (q *qos0Cache) get(version byte, retain bool) []byte {
	v, r := 0, 0
	if version == version5 {
		v = 1
	}
	if retain {
		r = 1
	}
	if q.pkts[v][r] == nil {
		q.pkts[v][r] = appendPublish(nil, version, q.m.Topic, q.m.Payload, 0, retain, false, 0, q.m.Properties, q.secs)
	}
	return q.pkts[v][r]
}

// route delivers a message to retained storage, the application and the subscribers. from is the
// connection whose loop runs the call, nil from other goroutines.
func (b *Broker) route(from *uringnet.Conn, m *Message, deadline int64) {
	if m.Retain {
		b.retainedMu.Lock()
		if len(m.Payload) == 0 {
			delete(b.retained, m.Topic) // an empty retained message clears the topic
		} else {
			cp := *m
			cp.Payload = append([]byte(nil), m.Payload...)
			b.retained[m.Topic] = &stored{m: &cp, deadline: deadline}
		}
		b.retainedMu.Unlock()
	}
	if m.ClientID != "" && b.cfg.OnPublish != nil {
		b.cfg.OnPublish(m)
	}
	secs, _ := remaining(deadline)
	cache := &qos0Cache{m: m, secs: secs}
	b.subs.match(m.Topic, mergeOpts, func(s *session, o subOpts) {
		if o.noLocal && s.id == m.ClientID {
			return
		}
		qos := m.QoS
		if o.qos < qos {
			qos = o.qos
		}
		retain := m.Retain && o.retainAsPublished
		if qos == 0 {
			s.mu.Lock()
			cl := s.client
			s.mu.Unlock()
			if cl != nil {
				b.send(from, cl, cache.get(cl.version, retain), true)
			}
			return
		}
		s.deliver1(from, m, retain, deadline)
	})
}

// deliver1 sends m with QoS 1, or queues it while the session is offline or its window is full.
func (s *session) deliver1(from *uringnet.Conn, m *Message, retain bool, deadline int64) {
	cp := *m
	cp.Payload = append([]byte(nil), m.Payload...)
	cp.QoS, cp.Retain = 1, retain
	st := &stored{m: &cp, deadline: deadline}
	s.mu.Lock()
	cl := s.client
	if cl == nil || len(s.inflight) >= s.window {
		if len(s.queue) >= s.b.cfg.MaxQueued {
			s.mu.Unlock()
			s.b.dropped.Add(1)
			return
		}
		s.queue = append(s.queue, st)
		s.mu.Unlock()
		return
	}
	pkt := s.startLocked(st, cl)
	s.mu.Unlock()
	if pkt != nil {
		s.b.send(from, cl, pkt, false)
	}
}

// startLocked assigns a packet id to st, records it as inflight and encodes it for cl; nil if the
// message expired or is larger than the client accepts (it is then discarded).
func (s *session) startLocked(st *stored, cl *client) []byte {
	for {
		s.nextID++
		if s.nextID != 0 && s.inflight[s.nextID] == nil {
			break
		}
	}
	id := s.nextID
	pkt := cl.encode(st, 1, st.m.Retain, false, id)
	if pkt == nil {
		s.b.expired.Add(1)
		return nil
	}
	if cl.maxOut > 0 && len(pkt) > cl.maxOut {
		s.b.dropped.Add(1)
		return nil
	}
	s.inflight[id] = st
	s.order = append(s.order, id)
	return pkt
}

// fillLocked starts queued messages while the window has room and appends them to out.
func (s *session) fillLocked(out []byte, cl *client) []byte {
	for len(s.queue) > 0 && len(s.inflight) < s.window {
		st := s.queue[0]
		s.queue[0] = nil
		s.queue = s.queue[1:]
		out = append(out, s.startLocked(st, cl)...)
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
			return h.fail(cl, 0, errProtocol, "first packet is not CONNECT")
		}
		return h.connect(cl, flags, body)
	}
	b, s, v := h.b, cl.s, cl.version
	switch typ {
	case typePublish:
		return h.publish(cl, flags, body)
	case typePuback:
		id, _, err := parseAck(body, v)
		if err != nil {
			return h.fail(cl, rcMalformed, err, "PUBACK")
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
		out := s.fillLocked(nil, cl)
		s.mu.Unlock()
		if len(out) > 0 {
			b.sent.Add(1)
			c.Write(out)
		}
	case typePubrel:
		if flags != 0x02 {
			return h.fail(cl, rcMalformed, errMalformed, "PUBREL flags")
		}
		id, _, err := parseAck(body, v)
		if err != nil {
			return h.fail(cl, rcMalformed, err, "PUBREL")
		}
		s.mu.Lock()
		known := s.inQoS2[id]
		delete(s.inQoS2, id)
		s.mu.Unlock()
		reason := byte(rcSuccess)
		if !known {
			reason = rcPacketIDNotFound
		}
		c.Write(appendAck(nil, v, typePubcomp, id, reason))
	case typeSubscribe:
		return h.subscribe(cl, flags, body)
	case typeUnsubscribe:
		id, filters, err := parseUnsubscribe(flags, body, v)
		if err != nil {
			return h.fail(cl, rcMalformed, err, "UNSUBSCRIBE")
		}
		codes := make([]byte, len(filters))
		for i, f := range filters {
			if !b.subs.remove(f, s) {
				codes[i] = rcNoSubscriptionExisted
			}
			s.mu.Lock()
			delete(s.filters, f)
			s.mu.Unlock()
		}
		c.Write(appendUnsuback(nil, v, id, codes))
	case typePingreq:
		c.Write([]byte{typePingresp << 4, 0})
	case typeDisconnect:
		reason, pp, err := parseDisconnect(flags, body, v)
		if err != nil {
			return h.fail(cl, rcMalformed, err, "DISCONNECT")
		}
		if pp.has[propSessionExpiry] {
			s.mu.Lock()
			wasZero := s.expiry == 0
			s.mu.Unlock()
			if wasZero && pp.sessionExpiry != 0 {
				return h.fail(cl, rcProtocolError, errProtocol, "session expiry set on DISCONNECT after 0 on CONNECT")
			}
			e := b.sessionExpiry(pp.sessionExpiry)
			s.mu.Lock()
			s.expiry = e
			s.mu.Unlock()
		}
		if reason != rcDisconnectWithWill {
			cl.graceful = true
			cl.will = nil // a normal disconnect discards the will
		}
		return uringnet.Close
	case typePubrec, typePubcomp:
		// Only for outgoing QoS 2, which is never granted
	case typeAuth:
		return h.fail(cl, rcProtocolError, errProtocol, "AUTH without enhanced authentication")
	default:
		return h.fail(cl, rcProtocolError, errProtocol, fmt.Sprintf("unexpected packet type %d", typ))
	}
	return uringnet.None
}

// fail closes a connection; a connected MQTT 5 client first gets a DISCONNECT with reason (if not 0).
func (h *handler) fail(cl *client, reason byte, err error, what string) uringnet.Action {
	h.b.log.Debug("mqtt: closing connection", "client", cl.clientID, "reason", what, "err", err)
	if reason != 0 && cl.version == version5 && cl.connected.Load() {
		cl.conn.Write(appendDisconnect(nil, reason))
	}
	return uringnet.Close
}

// requestedExpiry converts an MQTT 5 session expiry interval (seconds) to a duration.
func requestedExpiry(secs uint32) time.Duration {
	if secs == 0xFFFFFFFF {
		return never
	}
	return time.Duration(secs) * time.Second
}

// sessionExpiry is requestedExpiry capped by Config.SessionExpiry.
func (b *Broker) sessionExpiry(secs uint32) time.Duration {
	e := requestedExpiry(secs)
	if b.cfg.SessionExpiry > 0 && (e == never || e > b.cfg.SessionExpiry) {
		e = b.cfg.SessionExpiry
	}
	return e
}

func (h *handler) connect(cl *client, flags byte, body []byte) uringnet.Action {
	b, c := h.b, cl.conn
	p, err := parseConnect(flags, body)
	if errors.Is(err, errBadProtocol) {
		c.Write(appendConnack(nil, version311, false, connBadProtocol, nil))
		return h.fail(cl, 0, err, "CONNECT")
	}
	if err != nil {
		return h.fail(cl, 0, err, "CONNECT")
	}
	v := p.version
	cl.version = v
	// refuse rejects the connection with a CONNACK (MQTT 3.1.1 code 0: close without one)
	refuse := func(code3, code5 byte, why string) uringnet.Action {
		if v == version5 {
			c.Write(appendConnack(nil, v, false, code5, nil))
		} else if code3 != 0 {
			c.Write(appendConnack(nil, v, false, code3, nil))
		}
		return h.fail(cl, 0, errProtocol, why)
	}
	if v == version5 && p.props.has[propAuthMethod] {
		return refuse(0, rcBadAuthMethod, "enhanced authentication is not supported")
	}
	id := p.clientID
	assigned := false
	if id == "" {
		if v != version5 && !p.cleanStart {
			return refuse(connIdentifierInvalid, rcClientIDInvalid, "empty client ID with a persistent session")
		}
		b.mu.Lock()
		b.autoID++
		id = "auto-" + strconv.FormatUint(b.autoID, 10)
		b.mu.Unlock()
		assigned = true
	}
	if b.cfg.Authenticate != nil && !b.cfg.Authenticate(id, p.username, p.password) {
		return refuse(connBadCredentials, rcBadCredentials, "authentication failed")
	}
	if p.will != nil && !validTopicName(p.will.Topic) {
		return refuse(0, rcTopicNameInvalid, "invalid will topic")
	}

	// How long the session outlives the connection
	var expiry time.Duration
	capped := false
	if v == version5 {
		expiry = b.sessionExpiry(p.props.sessionExpiry)
		capped = expiry != requestedExpiry(p.props.sessionExpiry)
	} else if !p.cleanStart {
		expiry = never
		if b.cfg.SessionExpiry > 0 {
			expiry = b.cfg.SessionExpiry
		}
	}
	window := b.cfg.MaxInflight
	if v == version5 && p.props.has[propReceiveMaximum] && int(p.props.receiveMaximum) < window {
		window = int(p.props.receiveMaximum)
	}
	if v == version5 && p.props.has[propMaximumPacketSize] {
		cl.maxOut = int(p.props.maximumPacketSize)
	}

	// Take over or discard an existing session with this client ID
	b.mu.Lock()
	s := b.sessions[id]
	present := false
	if s != nil {
		s.mu.Lock()
		old := s.client
		s.client = nil
		if s.willTimer != nil {
			s.willTimer.Stop() // the client came back before its delayed will
			s.willTimer = nil
		}
		s.mu.Unlock()
		if old != nil {
			oc, ov := old.conn, old.version
			oc.Post(func() {
				if ov == version5 {
					oc.Write(appendDisconnect(nil, rcSessionTakenOver))
				}
				oc.Close() // its OnClose sees that it no longer owns the session
			})
		}
		if p.cleanStart {
			b.dropSessionLocked(s)
			s = nil
		} else {
			present = true
		}
	}
	if s == nil {
		s = newSession(b, id)
		b.sessions[id] = s
	}
	s.mu.Lock()
	s.client = cl
	s.expiry = expiry
	s.window = window
	// A resumed session resends its unacknowledged messages (DUP set), then queued ones
	var out []byte
	for _, mid := range s.order {
		st := s.inflight[mid]
		out = append(out, cl.encode(st, 1, st.m.Retain, true, mid)...)
	}
	out = s.fillLocked(out, cl)
	s.mu.Unlock()
	b.mu.Unlock()

	cl.s, cl.clientID, cl.will = s, id, p.will
	cl.willDelay = time.Duration(p.willDelay) * time.Second
	cl.keepAlive = time.Duration(p.keepAlive) * time.Second
	cl.connected.Store(true)

	var w *propWriter
	if v == version5 {
		w = &propWriter{}
		if assigned {
			w.string(propAssignedClientID, id)
		}
		if capped {
			w.uint32(propSessionExpiry, uint32(expiry/time.Second))
		}
		if b.cfg.MaxInflight < 65535 {
			w.uint16(propReceiveMaximum, uint16(b.cfg.MaxInflight))
		}
		w.uint32(propMaximumPacketSize, uint32(b.cfg.MaxPacketSize))
		if b.cfg.TopicAliasMaximum > 0 {
			w.uint16(propTopicAliasMaximum, uint16(b.cfg.TopicAliasMaximum))
		}
		w.byte(propSubIDAvailable, 0)
		w.byte(propSharedSubAvailable, 0)
	}
	c.Write(appendConnack(nil, v, present, connAccepted, w))
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
	b, c, s, v := h.b, cl.conn, cl.s, cl.version
	p, err := parsePublish(flags, body, v)
	if err != nil {
		return h.fail(cl, rcMalformed, err, "PUBLISH")
	}
	if v == version5 {
		if len(p.props.subscriptionIDs) > 0 {
			return h.fail(cl, rcProtocolError, errProtocol, "subscription identifier in PUBLISH")
		}
		if p.props.has[propTopicAlias] {
			a := p.props.topicAlias
			if a == 0 || int(a) > b.cfg.TopicAliasMaximum {
				return h.fail(cl, rcTopicAliasInvalid, errProtocol, "topic alias out of range")
			}
			if p.topic != "" {
				if cl.aliases == nil {
					cl.aliases = make(map[uint16]string)
				}
				cl.aliases[a] = p.topic
			} else if t, ok := cl.aliases[a]; ok {
				p.topic = t
			} else {
				return h.fail(cl, rcProtocolError, errProtocol, "unknown topic alias")
			}
		}
	}
	if !validTopicName(p.topic) {
		return h.fail(cl, rcTopicNameInvalid, errProtocol, "PUBLISH to an invalid topic")
	}
	b.received.Add(1)
	allowed := b.cfg.AuthorizePublish == nil || b.cfg.AuthorizePublish(cl.clientID, p.topic)
	ackReason := byte(rcSuccess)
	if !allowed {
		ackReason = rcNotAuthorized // MQTT 3.1.1 clients get a plain acknowledgement
	}
	m := &Message{Topic: p.topic, Payload: p.payload, QoS: p.qos, Retain: p.retain, ClientID: cl.clientID}
	var deadline int64
	if v == version5 {
		m.Properties = p.props.messageProps()
		if p.props.has[propMessageExpiry] {
			m.Expiry = time.Duration(p.props.messageExpiry) * time.Second
			deadline = deadlineOf(m.Expiry)
		}
	}
	switch p.qos {
	case 0:
		if allowed {
			b.route(c, m, deadline)
		}
	case 1:
		if allowed {
			b.route(c, m, deadline)
		}
		c.Write(appendAck(nil, v, typePuback, p.id, ackReason))
	case 2:
		s.mu.Lock()
		dup := s.inQoS2[p.id]
		if allowed {
			s.inQoS2[p.id] = true
		}
		s.mu.Unlock()
		if allowed && !dup { // deliver once: a resent PUBLISH before PUBREL is not routed again
			b.route(c, m, deadline)
		}
		c.Write(appendAck(nil, v, typePubrec, p.id, ackReason))
	}
	return uringnet.None
}

func (h *handler) subscribe(cl *client, flags byte, body []byte) uringnet.Action {
	b, c, s, v := h.b, cl.conn, cl.s, cl.version
	id, subs, pp, err := parseSubscribe(flags, body, v)
	if err != nil {
		return h.fail(cl, rcMalformed, err, "SUBSCRIBE")
	}
	if len(pp.subscriptionIDs) > 0 {
		return h.fail(cl, rcSubIDsNotSupported, errProtocol, "subscription identifiers are not supported")
	}
	refused := func(code5 byte) byte {
		if v == version5 {
			return code5
		}
		return rcUnspecified // 0x80, the only failure code of MQTT 3.1.1
	}
	codes := make([]byte, len(subs))
	isNew := make([]bool, len(subs))
	for i, sub := range subs {
		switch {
		case strings.HasPrefix(sub.filter, "$share/"):
			codes[i] = refused(rcSharedSubNotSupported)
			continue
		case !validFilter(sub.filter):
			codes[i] = refused(rcTopicFilterInvalid)
			continue
		case b.cfg.AuthorizeSubscribe != nil && !b.cfg.AuthorizeSubscribe(cl.clientID, sub.filter):
			codes[i] = refused(rcNotAuthorized)
			continue
		}
		granted := sub.qos
		if granted > 1 {
			granted = 1
		}
		codes[i] = granted
		o := subOpts{qos: granted, noLocal: sub.noLocal, retainAsPublished: sub.retainAsPublished}
		isNew[i] = b.subs.add(sub.filter, s, o)
		s.mu.Lock()
		s.filters[sub.filter] = o
		s.mu.Unlock()
	}
	c.Write(appendSuback(nil, v, id, codes))
	// Retained messages matching the new subscriptions follow the SUBACK (retain handling 0: always,
	// 1: only for a new subscription, 2: never)
	for i, sub := range subs {
		if codes[i] >= 0x80 || sub.retainHandling == 2 || (sub.retainHandling == 1 && !isNew[i]) {
			continue
		}
		for _, st := range b.retainedMatching(sub.filter) {
			qos := st.m.QoS
			if codes[i] < qos {
				qos = codes[i]
			}
			if qos == 0 {
				if pkt := cl.encode(st, 0, true, false, 0); pkt != nil {
					b.send(c, cl, pkt, false)
				}
			} else {
				s.deliver1(c, st.m, true, st.deadline)
			}
		}
	}
	return uringnet.None
}

func (b *Broker) retainedMatching(filter string) []*stored {
	b.retainedMu.RLock()
	defer b.retainedMu.RUnlock()
	var out []*stored
	for topic, st := range b.retained {
		if _, expired := remaining(st.deadline); !expired && matchFilter(filter, topic) {
			out = append(out, st)
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
	s := cl.s
	s.mu.Lock()
	owner := s.client == cl // false after a takeover by a newer connection
	if owner {
		s.client = nil
		s.offlineSince = now()
	}
	expiry := s.expiry
	s.mu.Unlock()

	// The will: at once, or after the will delay but no later than the end of the session, unless
	// the client reconnects first. After a takeover the session goes on, so a delayed will is dropped.
	if w := cl.will; w != nil && !cl.graceful {
		msg := *w
		msg.ClientID = cl.clientID
		delay := cl.willDelay
		if owner && expiry != never && expiry < delay {
			delay = expiry
		}
		switch {
		case delay <= 0:
			b.route(c, &msg, deadlineOf(msg.Expiry))
		case owner:
			s.mu.Lock()
			s.willTimer = time.AfterFunc(delay, func() {
				s.mu.Lock()
				offline := s.client == nil
				s.willTimer = nil
				s.mu.Unlock()
				if offline {
					b.route(nil, &msg, deadlineOf(msg.Expiry))
				}
			})
			s.mu.Unlock()
		}
	}
	if owner && expiry == 0 {
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
	s.filters = make(map[string]subOpts)
	s.queue, s.inflight, s.order = nil, make(map[uint16]*stored), nil
	s.mu.Unlock()
	for f := range filters {
		b.subs.remove(f, s)
	}
}

// watch enforces CONNECT and keepalive timeouts and removes expired sessions and retained messages.
func (b *Broker) watch() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for tick := 1; ; tick++ {
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
				conn, v := cl.conn, cl.version
				conn.Post(func() {
					if v == version5 {
						conn.Write(appendDisconnect(nil, rcKeepAliveTimeout))
					}
					conn.Close()
				})
			}
		}
		for _, s := range b.sessions {
			s.mu.Lock()
			expired := s.client == nil && s.expiry > 0 && time.Duration(n-s.offlineSince) > s.expiry
			s.mu.Unlock()
			if expired {
				b.dropSessionLocked(s) // deleting from a map while ranging over it is allowed
				b.sessionsExpired.Add(1)
			}
		}
		b.mu.Unlock()
		if tick%20 == 0 {
			b.retainedMu.Lock()
			for topic, st := range b.retained {
				if _, expired := remaining(st.deadline); expired {
					delete(b.retained, topic)
				}
			}
			b.retainedMu.Unlock()
		}
	}
}

type discard struct{}

func (discard) Enabled(context.Context, slog.Level) bool  { return false }
func (discard) Handle(context.Context, slog.Record) error { return nil }
func (d discard) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discard) WithGroup(string) slog.Handler           { return d }
