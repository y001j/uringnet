//go:build linux
// +build linux

package modbus

import (
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/y001j/uringnet"
)

// DTUConfig configures a DTUServer.
type DTUConfig struct {
	LinkConfig
	// Register parses the registration packet a DTU sends when it connects. Given the bytes received
	// so far, it returns the DTU's ID and the packet's length, n == 0 to wait for more bytes, or an
	// error to close the connection. nil: no registration packet, the ID is the remote address.
	// RegisterFixed and RegisterFixedHex cover the common fixed-length IDs (IMEI, serial number).
	Register func(buf []byte) (id string, n int, err error)
	// RegisterTimeout closes connections that send no registration packet within it (default 10s).
	RegisterTimeout time.Duration
	// Heartbeat is the heartbeat packet the DTUs send while idle. It is discarded wherever it
	// appears between frames. Empty: no heartbeat.
	Heartbeat []byte
	// Authorize, if set, accepts or rejects a registered DTU before it goes online.
	Authorize func(id string, remote string) bool
	// OnOnline and OnOffline report DTUs coming and going. They run on an event loop and must not
	// block; start polling from a goroutine. A DTU that connects again with the same ID replaces
	// its old connection (OnOffline for the old Link, then OnOnline for the new one).
	OnOnline  func(l *Link)
	OnOffline func(l *Link, err error)
}

// RegisterFixed returns a Register function for registration packets of n bytes used as the ID
// as they are (for example a 15-digit IMEI sent as ASCII).
func RegisterFixed(n int) func([]byte) (string, int, error) {
	return func(buf []byte) (string, int, error) {
		if len(buf) < n {
			return "", 0, nil
		}
		return string(buf[:n]), n, nil
	}
}

// RegisterFixedHex is RegisterFixed for binary IDs, which are returned hex-encoded.
func RegisterFixedHex(n int) func([]byte) (string, int, error) {
	return func(buf []byte) (string, int, error) {
		if len(buf) < n {
			return "", 0, nil
		}
		return hex.EncodeToString(buf[:n]), n, nil
	}
}

// ErrReplaced is the error OnOffline reports for a connection replaced by a newer one with the
// same DTU ID.
var ErrReplaced = errors.New("modbus: DTU connected again")

var errNotAuthorized = errors.New("modbus: DTU not authorized")

// DTUServer accepts connections from DTUs — devices that connect to a central server and carry
// Modbus over that connection (4G/Ethernet DTUs, serial device servers in client mode) — and acts
// as the Modbus master on each one. Serve it with uringnet.Listen("tcp", ":9000", s.Handler()).
// Usually the framing is RTUOverTCP, for DTUs in transparent mode in front of RS-485 devices.
type DTUServer struct {
	cfg DTUConfig

	mu    sync.Mutex
	links map[string]*Link
}

// NewDTUServer creates a DTU server.
func NewDTUServer(cfg DTUConfig) *DTUServer {
	cfg.LinkConfig.setDefaults()
	if cfg.RegisterTimeout <= 0 {
		cfg.RegisterTimeout = 10 * time.Second
	}
	return &DTUServer{cfg: cfg, links: make(map[string]*Link)}
}

// Handler returns the uringnet handler serving DTU connections.
func (s *DTUServer) Handler() uringnet.Handler { return &dtuHandler{s: s} }

// Link returns the online DTU with this ID, nil if it is offline.
func (s *DTUServer) Link(id string) *Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.links[id]
}

// IDs returns the IDs of the online DTUs, sorted.
func (s *DTUServer) IDs() []string {
	s.mu.Lock()
	ids := make([]string, 0, len(s.links))
	for id := range s.links {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	sort.Strings(ids)
	return ids
}

// dtuConn is the state of one DTU connection.
type dtuConn struct {
	link  *Link // nil until registered
	timer *time.Timer
	err   error // why the server closed it
}

type dtuHandler struct {
	uringnet.BuiltinHandler
	s *DTUServer
}

func (h *dtuHandler) OnOpen(c *uringnet.Conn) uringnet.Action {
	dc := &dtuConn{}
	c.SetContext(dc)
	if h.s.cfg.Register == nil {
		return h.online(c, dc, c.RemoteAddr().String())
	}
	dc.timer = time.AfterFunc(h.s.cfg.RegisterTimeout, func() { c.AsyncClose() })
	return uringnet.None
}

func (h *dtuHandler) online(c *uringnet.Conn, dc *dtuConn, id string) uringnet.Action {
	s := h.s
	if s.cfg.Authorize != nil && !s.cfg.Authorize(id, c.RemoteAddr().String()) {
		dc.err = errNotAuthorized
		return uringnet.Close
	}
	l := newLink(s.cfg.LinkConfig, id)
	l.heartbeat = s.cfg.Heartbeat
	dc.link = l
	s.mu.Lock()
	old := s.links[id]
	s.links[id] = l
	s.mu.Unlock()
	if old != nil {
		old.mu.Lock()
		old.endErr = ErrReplaced
		old.mu.Unlock()
		old.Close()
	}
	l.attach(c)
	if s.cfg.OnOnline != nil {
		s.cfg.OnOnline(l)
	}
	return uringnet.None
}

func (h *dtuHandler) OnTraffic(c *uringnet.Conn, _ []byte) uringnet.Action {
	dc := c.Context().(*dtuConn)
	if dc.link == nil {
		buf, _ := c.Peek(-1)
		id, n, err := h.s.cfg.Register(buf)
		if err != nil {
			dc.err = err
			return uringnet.Close
		}
		if n == 0 {
			return uringnet.None
		}
		dc.timer.Stop()
		c.Discard(n)
		if a := h.online(c, dc, id); a != uringnet.None {
			return a
		}
		if c.InboundBuffered() == 0 {
			return uringnet.None
		}
	}
	return dc.link.onData(c)
}

func (h *dtuHandler) OnClose(c *uringnet.Conn, err error) {
	dc := c.Context().(*dtuConn)
	if dc.timer != nil {
		dc.timer.Stop()
	}
	l := dc.link
	if l == nil {
		return
	}
	s := h.s
	s.mu.Lock()
	if s.links[l.id] == l {
		delete(s.links, l.id)
	}
	s.mu.Unlock()
	l.detach(c)
	l.mu.Lock()
	if l.endErr != nil {
		err = l.endErr
	}
	l.mu.Unlock()
	if dc.err != nil {
		err = dc.err
	}
	if s.cfg.OnOffline != nil {
		s.cfg.OnOffline(l, err)
	}
}
