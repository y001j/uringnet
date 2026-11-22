//go:build linux
// +build linux

package uringnet

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	mrand "math/rand"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// sockoptRecorder records socket options of each connection in OnOpen.
type sockoptRecorder struct {
	BuiltinHandler
	mu   sync.Mutex
	opts []map[string]int
}

func (h *sockoptRecorder) OnOpen(c *Conn) Action {
	get := func(level, opt int) int { v, _ := unix.GetsockoptInt(c.Fd(), level, opt); return v }
	h.mu.Lock()
	h.opts = append(h.opts, map[string]int{
		"keepalive": get(unix.SOL_SOCKET, unix.SO_KEEPALIVE),
		"keepidle":  get(unix.IPPROTO_TCP, unix.TCP_KEEPIDLE),
		"keepintvl": get(unix.IPPROTO_TCP, unix.TCP_KEEPINTVL),
		"rcvbuf":    get(unix.SOL_SOCKET, unix.SO_RCVBUF),
		"sndbuf":    get(unix.SOL_SOCKET, unix.SO_SNDBUF),
	})
	h.mu.Unlock()
	return None
}

func (h *sockoptRecorder) recorded() []map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]int(nil), h.opts...)
}

// Keepalive and buffer sizes reach accepted connections (inherited from the listening socket) and dialed ones.
func TestKeepAliveAndSocketBuffers(t *testing.T) {
	const buf = 96 << 10
	h := &sockoptRecorder{}
	_, addr := serve(t, h, WithRings(2), WithTCPKeepAlive(42*time.Second), WithSocketBuffers(buf, buf))
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cl := newTestClient(t, h, WithRings(1), WithTCPKeepAlive(42*time.Second), WithSocketBuffers(buf, buf))
	if _, err := cl.Dial(context.Background(), "tcp4", addr, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(h.recorded()) == 3 }, "accepted and dialed connections")
	for i, o := range h.recorded() {
		// The kernel doubles SO_RCVBUF/SO_SNDBUF values for its bookkeeping
		if o["keepalive"] != 1 || o["keepidle"] != 42 || o["keepintvl"] != 42 || o["rcvbuf"] < buf || o["sndbuf"] < buf {
			t.Fatalf("connection %d: socket options %v", i, o)
		}
	}
}

// slowHandler sleeps in OnTraffic when asked to.
type slowHandler struct{ BuiltinHandler }

func (slowHandler) OnTraffic(c *Conn, data []byte) Action {
	if string(data) == "slow" {
		time.Sleep(120 * time.Millisecond)
	}
	c.Write(data)
	return None
}

func TestSlowCallbackWarning(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	s, addr := serve(t, slowHandler{}, WithRings(1), WithSlowCallbackWarning(50*time.Millisecond), WithLogger(logger))
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, msg := range []string{"fast", "slow", "fast"} {
		c.Write([]byte(msg))
		readN(t, c, len(msg))
	}
	if n := s.Stats().SlowCallbacks; n != 1 {
		t.Fatalf("SlowCallbacks = %d, want 1", n)
	}
	out := string(logs.Bytes())
	if !bytes.Contains([]byte(out), []byte("slow callback blocked the event loop")) {
		t.Fatalf("no slow-callback warning in %q", out)
	}
	if !bytes.Contains([]byte(out), []byte("callback still running")) {
		t.Fatalf("the watchdog did not report the running callback: %q", out)
	}
}

func TestLoadBalancing(t *testing.T) {
	for _, tc := range []struct {
		name string
		lb   LoadBalancing
	}{{"round-robin", BalanceRoundRobin}, {"least-connections", BalanceLeastConnections}, {"source-addr-hash", BalanceSourceAddrHash}} {
		t.Run(tc.name, func(t *testing.T) {
			s, addr := serve(t, &apiEcho{}, WithRings(4), WithLoadBalancing(tc.lb))
			var conns []net.Conn
			defer func() {
				for _, c := range conns {
					c.Close()
				}
			}()
			for i := 0; i < 40; i++ {
				c, err := net.Dial("tcp4", addr)
				if err != nil {
					t.Fatal(err)
				}
				conns = append(conns, c)
				waitFor(t, func() bool { return s.Connections() == int64(i+1) }, "connection opened")
			}
			var per []int64
			for _, rn := range s.rings {
				per = append(per, rn.Connections())
			}
			switch tc.lb {
			case BalanceSourceAddrHash: // every connection comes from 127.0.0.1
				ones := 0
				for _, n := range per {
					if n == 40 {
						ones++
					}
				}
				if ones != 1 {
					t.Fatalf("connections per ring %v, want all on one ring", per)
				}
			default:
				for _, n := range per {
					if n != 10 {
						t.Fatalf("connections per ring %v, want 10 each", per)
					}
				}
			}
			// Connections handed to other rings work normally
			for i, c := range conns[:8] {
				msg := []byte{byte(i), 'x', 'y'}
				c.Write(msg)
				if got := readN(t, c, len(msg)); !bytes.Equal(got, msg) {
					t.Fatalf("echo on connection %d: %q", i, got)
				}
			}
		})
	}
}

// lengthParser parses 2-byte length-prefixed messages with Peek and Next and echoes them.
type lengthParser struct {
	BuiltinHandler
	messages int
	mu       sync.Mutex
}

func (h *lengthParser) OnTraffic(c *Conn, _ []byte) Action {
	for c.InboundBuffered() >= 2 {
		hdr, _ := c.Peek(2)
		n := int(binary.BigEndian.Uint16(hdr))
		if c.InboundBuffered() < 2+n {
			break // wait for the rest; it is kept for the next OnTraffic
		}
		msg, err := c.Next(2 + n)
		if err != nil {
			panic(err)
		}
		c.Write(msg)
		h.mu.Lock()
		h.messages++
		h.mu.Unlock()
	}
	return None
}

func TestInboundAPI(t *testing.T) {
	h := &lengthParser{}
	_, addr := serve(t, h, WithRings(1), WithBuffers(16, 2048))
	rng := mrand.New(mrand.NewSource(3))
	var stream []byte
	for i := 0; i < 1000; i++ {
		payload := make([]byte, rng.Intn(5000))
		rng.Read(payload)
		stream = binary.BigEndian.AppendUint16(stream, uint16(len(payload)))
		stream = append(stream, payload...)
	}
	if err := echoChunked(addr, stream, 11); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.messages != 1000 {
		t.Fatalf("parsed %d messages, want 1000", h.messages)
	}
}

// A handler that only uses OnTraffic's data keeps nothing between callbacks.
func TestInboundAPINotUsedKeepsNothing(t *testing.T) {
	s, addr := serve(t, &apiEcho{}, WithRings(1))
	if err := echoChunked(addr, bytes.Repeat([]byte("z"), 100000), 5); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("abc"))
	readN(t, c, 3)
	onLoop(t, s.rings[0], func() {
		for _, cc := range s.rings[0].conns {
			if len(cc.api.in) != 0 || cap(cc.api.in) != 0 {
				t.Errorf("connection kept %d bytes", len(cc.api.in))
			}
		}
	})
}

func TestUDPMulticast(t *testing.T) {
	group := netip.MustParseAddr("239.255.77.1")
	got := make(chan string, 1)
	s := servePacket(t, "udp4", group.String()+":0", PacketHandlerFunc(func(pc *PacketConn, data []byte, from netip.AddrPort) {
		select {
		case got <- string(data):
		default:
		}
	}), WithRings(2))
	port := s.Addr().(*net.UDPAddr).Port
	c, err := net.Dial("udp4", netip.AddrPortFrom(group, uint16(port)).String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("to the group")); err != nil {
		if errors.Is(err, syscall.ENETUNREACH) {
			t.Skipf("no route for multicast in this environment: %v", err)
		}
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		if msg != "to the group" {
			t.Fatalf("got %q", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("multicast datagram not received")
	}
	if !s.rings[0].ownsSocket && s.rings[0].SocketFd != s.rings[1].SocketFd {
		t.Fatal("rings should share one socket for a multicast group")
	}
}
