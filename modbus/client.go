//go:build linux
// +build linux

package modbus

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/y001j/uringnet"
)

// ClientConfig configures a Client.
type ClientConfig struct {
	LinkConfig
	// DialTimeout bounds each connection attempt (default 5s).
	DialTimeout time.Duration
	// ReconnectMin and ReconnectMax bound the wait between connection attempts, which doubles
	// after each failure (defaults 100ms and 30s).
	ReconnectMin, ReconnectMax time.Duration
	// OnConnect and OnDisconnect report a Link's connection coming and going. They run on an
	// event loop or the Link's connection goroutine and must not block.
	OnConnect    func(l *Link)
	OnDisconnect func(l *Link, err error)
}

// Client is a Modbus master for devices it connects to. Its connections share a set of uringnet
// event loops, so one Client can poll thousands of devices.
type Client struct {
	cfg ClientConfig
	u   *uringnet.Client

	mu     sync.Mutex
	links  map[*Link]struct{}
	closed bool
}

// NewClient creates a client. opts configure its event loops (uringnet.WithRings, ...).
func NewClient(cfg ClientConfig, opts ...uringnet.Option) (*Client, error) {
	cfg.LinkConfig.setDefaults()
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.ReconnectMin <= 0 {
		cfg.ReconnectMin = 100 * time.Millisecond
	}
	if cfg.ReconnectMax < cfg.ReconnectMin {
		cfg.ReconnectMax = 30 * time.Second
		if cfg.ReconnectMax < cfg.ReconnectMin {
			cfg.ReconnectMax = cfg.ReconnectMin
		}
	}
	c := &Client{cfg: cfg, links: make(map[*Link]struct{})}
	u, err := uringnet.NewClient(&linkHandler{c: c}, opts...)
	if err != nil {
		return nil, err
	}
	c.u = u
	return c, nil
}

// Dial returns a Link to address ("host:port") and connects it in the background; it reconnects
// whenever the connection is lost, until the Link or the Client is closed. Requests can be made at
// once: they wait for the connection.
func (c *Client) Dial(address string) *Link {
	l := newLink(c.cfg.LinkConfig, address)
	l.client = c
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		l.Close()
		return l
	}
	c.links[l] = struct{}{}
	c.mu.Unlock()
	go c.run(l)
	return l
}

// Close closes every Link and the event loops.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	links := c.links
	c.links = nil
	c.mu.Unlock()
	for l := range links {
		l.Close()
	}
	return c.u.Close()
}

func (l *Link) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// run keeps a Link connected.
func (c *Client) run(l *Link) {
	defer func() {
		c.mu.Lock()
		delete(c.links, l)
		c.mu.Unlock()
	}()
	wait := c.cfg.ReconnectMin
	for !l.isClosed() {
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.DialTimeout)
		_, err := c.u.Dial(ctx, "tcp", l.id, l) // OnOpen attaches the connection
		cancel()
		if err != nil {
			if errors.Is(err, uringnet.ErrServerClosed) {
				return
			}
			if c.cfg.OnDisconnect != nil {
				c.cfg.OnDisconnect(l, err)
			}
			select {
			case <-time.After(wait):
			case <-l.down: // closed
			}
			if wait *= 2; wait > c.cfg.ReconnectMax {
				wait = c.cfg.ReconnectMax
			}
			continue
		}
		wait = c.cfg.ReconnectMin
		<-l.down // the connection ended, or the Link was closed
	}
}

// linkHandler serves a Client's connections; each connection's context is its Link.
type linkHandler struct {
	uringnet.BuiltinHandler
	c *Client
}

func (h *linkHandler) OnOpen(conn *uringnet.Conn) uringnet.Action {
	l := conn.Context().(*Link)
	l.attach(conn)
	if h.c.cfg.OnConnect != nil {
		h.c.cfg.OnConnect(l)
	}
	return uringnet.None
}

func (h *linkHandler) OnTraffic(conn *uringnet.Conn, _ []byte) uringnet.Action {
	return conn.Context().(*Link).onData(conn)
}

func (h *linkHandler) OnClose(conn *uringnet.Conn, err error) {
	l := conn.Context().(*Link)
	l.detach(conn)
	if h.c.cfg.OnDisconnect != nil && !l.isClosed() {
		h.c.cfg.OnDisconnect(l, err)
	}
}
