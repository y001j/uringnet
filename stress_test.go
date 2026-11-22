//go:build linux
// +build linux

package uringnet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	mrand "math/rand"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// splitEcho writes each chunk back in up to three slices: several in-place sends from one receive buffer, and the queued-then-copied path.
type splitEcho struct {
	BuiltinHandler
	opened, closed atomic.Int64
}

func (h *splitEcho) OnOpen(*Conn) Action { h.opened.Add(1); return None }

func (h *splitEcho) OnClose(*Conn, error) { h.closed.Add(1) }

func (h *splitEcho) OnTraffic(c *Conn, data []byte) Action {
	switch n := len(data); {
	case n >= 3:
		c.Write(data[:n/3])
		c.Write(data[n/3 : 2*n/3])
		c.Write(data[2*n/3:])
	default:
		c.Write(data)
	}
	return None
}

var (
	fuzzOnce  sync.Once
	fuzzAddrs []string
)

// fuzzServers starts servers with several configurations, shared by the whole test process.
func fuzzServers(t testing.TB) []string {
	fuzzOnce.Do(func() {
		configs := [][]Option{
			// Few, small buffers: frequent -ENOBUFS, receive pauses and resumes
			{WithRings(1), WithBuffers(8, 512), WithRecvBundle(BundleOff)},
			// Incremental buffers: one buffer shared by several connections and slices
			{WithRings(2), WithBuffers(4, 4096), WithIncrementalBuffers()},
			// Very low zero-copy threshold: most in-place sends use SEND_ZC (still correct on loopback, just no gain)
			{WithRings(1), WithBuffers(16, 2048), WithZeroCopy(256)},
		}
		for _, opts := range configs {
			s, err := Listen("tcp4", "127.0.0.1:0", &splitEcho{}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			go s.Serve()
			fuzzAddrs = append(fuzzAddrs, s.Addr().String())
		}
	})
	if len(fuzzAddrs) == 0 {
		t.Fatal("fuzz servers not running")
	}
	return fuzzAddrs
}

// FuzzEcho: for any data and any chunking, the echoed stream must equal what was sent.
// Run: go test -run '^$' -fuzz FuzzEcho -fuzztime 5m -fuzzminimizetime 5s
// (when it finds a new input the engine spends up to 60s minimizing it by default, without counting executions, which looks like a hang)
func FuzzEcho(f *testing.F) {
	f.Add([]byte("hello"), uint64(1), uint8(1))
	f.Add(bytes.Repeat([]byte{0xab}, 5000), uint64(7), uint8(3))
	f.Add(make([]byte, 70000), uint64(42), uint8(16))
	f.Fuzz(func(t *testing.T, data []byte, seed uint64, conns uint8) {
		if len(data) == 0 {
			return
		}
		if len(data) > 256<<10 {
			data = data[:256<<10]
		}
		n := int(conns%8) + 1
		for _, addr := range fuzzServers(t) {
			var wg sync.WaitGroup
			errs := make(chan error, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					errs <- echoChunked(addr, data, seed+uint64(i))
				}(i)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("%s: %v", addr, err)
				}
			}
		}
	})
}

// echoChunked sends data in random chunk sizes chosen by seed (pausing now and then) and checks what comes back.
func echoChunked(addr string, data []byte, seed uint64) error {
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	rng := mrand.New(mrand.NewSource(int64(seed)))
	writeErr := make(chan error, 1)
	go func() {
		for rest := data; len(rest) > 0; {
			k := 1 + rng.Intn(4096)
			if k > len(rest) {
				k = len(rest)
			}
			if _, err := c.Write(rest[:k]); err != nil {
				writeErr <- err
				return
			}
			rest = rest[k:]
			if rng.Intn(16) == 0 {
				time.Sleep(time.Duration(rng.Intn(200)) * time.Microsecond)
			}
		}
		writeErr <- nil
	}()
	got := make([]byte, len(data))
	c.SetReadDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		return fmt.Errorf("read %d bytes: %w", len(data), err)
	}
	if err := <-writeErr; err != nil {
		return err
	}
	if !bytes.Equal(got, data) {
		i := 0
		for i < len(got) && got[i] == data[i] {
			i++
		}
		return fmt.Errorf("echoed data differs at byte %d of %d", i, len(data))
	}
	return nil
}

func openFds(t testing.TB) int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// TestStressChurn: many connections opened and closed over and over, mixing normal echo, RST, half-close and write-only sessions;
// afterwards no fds, connections or receive buffers may have leaked. URINGNET_STRESS sets the duration (e.g. 60s), default 2s.
func TestStressChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	dur := 2 * time.Second
	if v := os.Getenv("URINGNET_STRESS"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
		dur = d
	}
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"normal", []Option{WithBuffers(16, 2048)}},
		{"incremental", []Option{WithBuffers(8, 16<<10), WithIncrementalBuffers()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fdsBefore := openFds(t)
			h := &splitEcho{}
			opts := append([]Option{WithRings(2), WithMaxWriteBuffer(64 << 10),
				WithIdleTimeout(time.Second), WithWriteTimeout(time.Second)}, tc.opts...)
			s, err := Listen("tcp4", "127.0.0.1:0", h, opts...)
			if err != nil {
				t.Fatal(err)
			}
			served := make(chan error, 1)
			go func() { served <- s.Serve() }()
			addr := s.Addr().String()

			var ok, errCount atomic.Int64
			var wg sync.WaitGroup
			deadline := time.Now().Add(dur)
			for w := 0; w < 32; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					rng := mrand.New(mrand.NewSource(int64(w)))
					for time.Now().Before(deadline) {
						if err := churnOnce(addr, rng); err != nil {
							errCount.Add(1)
							t.Errorf("worker %d: %v", w, err)
							return
						}
						ok.Add(1)
					}
				}(w)
			}
			wg.Wait()

			// All clients are gone: every server connection must be freed and every receive buffer returned
			waitFor(t, func() bool { return s.Connections() == 0 }, "all connections closed")
			for _, rn := range s.rings {
				var held, pinned, starved int
				onLoop(t, rn, func() { held, pinned, starved = rn.held, rn.incPinned, len(rn.starved) })
				if held != 0 || pinned != 0 || starved != 0 {
					t.Errorf("ring %d: held=%d incPinned=%d starved=%d after all connections closed",
						rn.index, held, pinned, starved)
				}
			}
			if o, c := h.opened.Load(), h.closed.Load(); o != c {
				t.Errorf("OnOpen %d times, OnClose %d times", o, c)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			<-served
			// Rings are released asynchronously, so the fds close a little later
			waitFor(t, func() bool { return openFds(t) <= fdsBefore }, "fds released")
			t.Logf("%d client sessions, %d connections", ok.Load(), h.opened.Load())
		})
	}
}

// churnOnce opens one connection and runs a random scenario.
func churnOnce(addr string, rng *mrand.Rand) error {
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		return err
	}
	tc := c.(*net.TCPConn)
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	msg := make([]byte, 1+rng.Intn(32<<10))
	rng.Read(msg)
	switch rng.Intn(4) {
	case 0: // echo and check
		go tc.Write(msg)
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(tc, got); err != nil {
			return fmt.Errorf("echo: %w", err)
		}
		if !bytes.Equal(got, msg) {
			return fmt.Errorf("echo: data differs")
		}
	case 1: // send, then reset at once
		tc.Write(msg)
		tc.SetLinger(0)
	case 2: // half-close, read everything back
		go func() { tc.Write(msg); tc.CloseWrite() }()
		got, err := io.ReadAll(tc)
		if err != nil {
			return fmt.Errorf("half-close: %w", err)
		}
		if !bytes.Equal(got, msg) {
			return fmt.Errorf("half-close: got %d bytes, want %d", len(got), len(msg))
		}
	case 3: // write without reading, then close
		for i := 0; i < 4; i++ {
			tc.Write(msg)
		}
	}
	return nil
}
