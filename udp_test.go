//go:build linux
// +build linux

package uringnet

import (
	"bytes"
	"crypto/rand"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// udpEcho 把数据报原样发回来源地址
var udpEcho = PacketHandlerFunc(func(pc *PacketConn, data []byte, from netip.AddrPort) {
	pc.WriteTo(data, from)
})

func servePacket(t *testing.T, network, address string, h PacketHandler, opts ...Option) *PacketServer {
	t.Helper()
	s, err := ListenPacket(network, address, h, opts...)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.Serve(); close(done) }()
	t.Cleanup(func() {
		s.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after Close")
		}
	})
	return s
}

// udpRoundTrip 发送一个数据报并等待回复
func udpRoundTrip(t *testing.T, c net.Conn, msg []byte) []byte {
	t.Helper()
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 70000)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("no reply to a %d byte datagram: %v", len(msg), err)
	}
	return buf[:n]
}

func TestUDPEcho(t *testing.T) {
	s := servePacket(t, "udp4", "127.0.0.1:0", udpEcho, WithRings(2))
	c, err := net.Dial("udp4", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sizes := []int{0, 1, 100, 1400, 8000, 16000}
	for _, n := range sizes {
		msg := make([]byte, n)
		rand.Read(msg)
		if got := udpRoundTrip(t, c, msg); !bytes.Equal(got, msg) {
			t.Fatalf("%d byte datagram: got %d bytes back", n, len(got))
		}
	}
	st := s.Stats()
	if st.PacketsReceived != uint64(len(sizes)) || st.PacketsSent != uint64(len(sizes)) {
		t.Fatalf("stats %+v", st)
	}
}

// 端口为 0、多个 ring：所有 socket 绑定同一个端口，内核把不同来源的数据报分到各个 ring
func TestUDPReusePortRings(t *testing.T) {
	s := servePacket(t, "udp4", "127.0.0.1:0", udpEcho, WithRings(4))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("udp4", s.Addr().String())
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			for j := 0; j < 10; j++ {
				msg := []byte("datagram")
				if got := udpRoundTrip(t, c, msg); !bytes.Equal(got, msg) {
					t.Errorf("got %q", got)
					return
				}
			}
		}()
	}
	wg.Wait()
	used := 0
	for _, rn := range s.rings {
		var n uint64
		onLoop(t, rn, func() { n = rn.packetsIn })
		if n > 0 {
			used++
		}
	}
	if used < 2 {
		t.Fatalf("datagrams went to %d of 4 rings", used)
	}
}

// 超过接收缓冲区的数据报被丢弃并计数，之后的数据报照常处理
func TestUDPTruncated(t *testing.T) {
	s := servePacket(t, "udp4", "127.0.0.1:0", udpEcho, WithRings(1), WithBuffers(16, 512))
	c, err := net.Dial("udp4", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(make([]byte, 1000))
	waitFor(t, func() bool { return s.Stats().PacketsTruncated == 1 }, "truncated datagram counted")
	if got := udpRoundTrip(t, c, []byte("small")); string(got) != "small" {
		t.Fatalf("got %q", got)
	}
}

// 绑定端口 0 的 PacketServer 当作 UDP 客户端：AsyncWriteTo 发送，OnPacket 收到回复
func TestUDPClient(t *testing.T) {
	echo, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			echo.WriteTo(bytes.ToUpper(buf[:n]), addr)
		}
	}()

	replies := make(chan string, 1)
	cl, err := ListenPacket("udp4", "127.0.0.1:0", PacketHandlerFunc(func(pc *PacketConn, data []byte, from netip.AddrPort) {
		replies <- string(data)
	}), WithRings(1))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if err := cl.Start(); err != nil {
		t.Fatal(err)
	}
	to := netip.MustParseAddrPort(echo.LocalAddr().String())
	sent := make(chan error, 1)
	if err := cl.AsyncWriteTo([]byte("hello"), to, func(err error) { sent <- err }); err != nil {
		t.Fatal(err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-replies:
		if r != "HELLO" {
			t.Fatalf("reply %q", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no reply")
	}
}

func TestUDPIPv6(t *testing.T) {
	l, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	l.Close()
	s := servePacket(t, "udp6", "[::1]:0", udpEcho, WithRings(1))
	c, err := net.Dial("udp6", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := udpRoundTrip(t, c, []byte("v6")); string(got) != "v6" {
		t.Fatalf("got %q", got)
	}
}

// 不开 SO_REUSEPORT 时多个 ring 共用一个 socket；反复创建和关闭不泄漏 fd，也不会把 fd 关两次
func TestUDPSharedSocketClose(t *testing.T) {
	before := openFds(t)
	for i := 0; i < 5; i++ {
		s, err := ListenPacket("udp4", "127.0.0.1:0", udpEcho, WithRings(2), WithReusePort(false))
		if err != nil {
			t.Fatal(err)
		}
		go s.Serve()
		c, err := net.Dial("udp4", s.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if got := udpRoundTrip(t, c, []byte("x")); string(got) != "x" {
			t.Fatalf("got %q", got)
		}
		c.Close()
		s.Close()
	}
	waitFor(t, func() bool { return openFds(t) <= before }, "fds released")
}

// 并发负载：16 个客户端各发 500 个随机大小的数据报，逐个校验回复
func TestUDPLoad(t *testing.T) {
	s := servePacket(t, "udp4", "127.0.0.1:0", udpEcho, WithRings(2), WithBuffers(64, 4096))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.Dial("udp4", s.Addr().String())
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			buf := make([]byte, 4096)
			for j := 0; j < 500; j++ {
				msg := make([]byte, 1+(i*500+j)%4000)
				rand.Read(msg)
				c.Write(msg)
				c.SetReadDeadline(time.Now().Add(2 * time.Second))
				n, err := c.Read(buf)
				if err != nil || !bytes.Equal(buf[:n], msg) {
					t.Errorf("client %d datagram %d: n=%d err=%v", i, j, n, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	if st := s.Stats(); st.PacketsReceived != 16*500 {
		t.Fatalf("stats %+v", st)
	}
}
