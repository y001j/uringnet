//go:build linux
// +build linux

package uringnet

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// testCert creates a self-signed certificate for localhost / 127.0.0.1 and a pool that trusts it.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "uringnet test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, pool
}

// ktlsLoaded reports whether the tls kernel module is available, i.e. whether kernel TLS should be used.
func ktlsLoaded() bool {
	_, err := os.Stat("/sys/module/tls")
	return err == nil
}

// forEachKTLS runs fn with kernel TLS enabled and disabled (user-space crypto/tls).
func forEachKTLS(t *testing.T, fn func(t *testing.T, ktls bool)) {
	for _, ktls := range []bool{true, false} {
		name := "userspace"
		if ktls {
			name = "ktls"
		}
		t.Run(name, func(t *testing.T) { fn(t, ktls) })
	}
}

// checkKTLS checks that connections used kernel TLS exactly when they should.
func checkKTLS(t *testing.T, st Stats, ktls bool, conns uint64) {
	t.Helper()
	want := uint64(0)
	if ktls && ktlsLoaded() {
		want = conns
	}
	if st.KernelTLSConnections != want {
		t.Fatalf("KernelTLSConnections = %d, want %d (tls module loaded: %v)", st.KernelTLSConnections, want, ktlsLoaded())
	}
}

// tlsEcho echoes plaintext and records what it saw.
type tlsEcho struct {
	apiEcho
	openedTLS atomic.Bool // OnOpen saw a completed handshake
}

func (h *tlsEcho) OnOpen(c *Conn) Action {
	if st, ok := c.TLSConnectionState(); ok && st.HandshakeComplete {
		h.openedTLS.Store(true)
	}
	return h.apiEcho.OnOpen(c)
}

// A standard library TLS client talks to a TLS server: large data stays intact, OnOpen runs after the
// handshake, and closing sends close_notify.
func TestTLSServer(t *testing.T) { forEachKTLS(t, testTLSServer) }

func testTLSServer(t *testing.T, ktls bool) {
	cert, pool := testCert(t)
	h := &tlsEcho{}
	s, addr := serve(t, h, WithRings(2), WithTLS(&tls.Config{Certificates: []tls.Certificate{cert}}), WithKTLS(ktls))

	c, err := tls.Dial("tcp4", addr, &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if v := c.ConnectionState().Version; v != tls.VersionTLS13 {
		t.Fatalf("negotiated %x, want TLS 1.3", v)
	}
	msg := make([]byte, 1<<20)
	rand.Read(msg)
	go c.Write(msg)
	got := make([]byte, len(msg))
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("echoed data differs")
	}
	if !h.openedTLS.Load() {
		t.Fatal("OnOpen ran before the handshake completed")
	}

	// The client half-closes (close_notify): the server sends what is left and closes with its own close_notify
	c.Write([]byte("bye"))
	c.CloseWrite()
	rest, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("reading after close_notify: %v (want a clean EOF)", err)
	}
	if string(rest) != "bye" {
		t.Fatalf("got %q after half-close", rest)
	}
	waitFor(t, func() bool { return len(h.closeErrs()) == 1 }, "OnClose for the TLS connection")
	if s.Connections() != 0 {
		t.Fatalf("%d connections after close", s.Connections())
	}
	if errs := h.closeErrs(); errs[0] != io.EOF {
		t.Fatalf("OnClose errors %v, want [EOF]", errs)
	}
	checkKTLS(t, s.Stats(), ktls, 1)
}

// A failed handshake closes the connection without OnOpen/OnClose and is counted.
func TestTLSHandshakeFailure(t *testing.T) {
	cert, _ := testCert(t)
	h := &tlsEcho{}
	s, addr := serve(t, h, WithRings(1), WithTLS(&tls.Config{Certificates: []tls.Certificate{cert}}))

	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")) // not TLS
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.ReadAll(c) // the server answers with an alert and closes
	waitFor(t, func() bool { return s.Stats().TLSHandshakeErrors == 1 }, "handshake error counted")
	waitFor(t, func() bool { return s.Connections() == 0 }, "connection closed")
	if h.openedTLS.Load() || len(h.closeErrs()) != 0 {
		t.Fatal("OnOpen/OnClose called for a connection whose handshake failed")
	}
}

// A TLS client: dial a standard library TLS server and a uringnet TLS server.
func TestTLSDial(t *testing.T) { forEachKTLS(t, testTLSDial) }

func testTLSDial(t *testing.T, ktls bool) {
	cert, pool := testCert(t)
	serverCfg := &tls.Config{Certificates: []tls.Certificate{cert}}

	std, err := tls.Listen("tcp4", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer std.Close()
	go func() {
		for {
			c, err := std.Accept()
			if err != nil {
				return
			}
			go io.Copy(c, c)
		}
	}()
	_, ours := serve(t, &apiEcho{}, WithRings(1), WithTLS(serverCfg), WithKTLS(ktls))

	for _, addr := range []string{std.Addr().String(), ours} {
		h := &collector{}
		cl := newTestClient(t, h, WithRings(1), WithDialTLS(&tls.Config{RootCAs: pool}), WithKTLS(ktls))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := cl.Dial(ctx, "tcp4", addr, nil) // ServerName defaults to 127.0.0.1, in the certificate
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		if st, ok := c.TLSConnectionState(); !ok || !st.HandshakeComplete {
			t.Fatalf("%s: Dial returned before the handshake", addr)
		}
		c.AsyncWrite([]byte("over TLS"), nil)
		waitFor(t, func() bool { return h.received() == "over TLS" }, "TLS echo")
		checkKTLS(t, cl.Stats(), ktls, 1)
	}

	// A certificate the client does not trust: Dial fails with the handshake error
	cl := newTestClient(t, &collector{}, WithRings(1), WithDialTLS(&tls.Config{}))
	if _, err := cl.Dial(context.Background(), "tcp4", ours, nil); err == nil {
		t.Fatal("Dial accepted an untrusted certificate")
	}
}

// Framing over TLS: frames are cut from the decrypted stream.
func TestTLSFramed(t *testing.T) { forEachKTLS(t, testTLSFramed) }

func testTLSFramed(t *testing.T, ktls bool) {
	cert, pool := testCert(t)
	lf := NewLengthField(4, binary.BigEndian, 1<<20)
	h := &frameEcho{}
	_, addr := serve(t, Framed(lf, h), WithRings(1), WithTLS(&tls.Config{Certificates: []tls.Certificate{cert}}), WithKTLS(ktls))

	c, err := tls.Dial("tcp4", addr, &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var stream []byte
	for i := 0; i < 100; i++ {
		stream = lf.AppendFrame(stream, bytes.Repeat([]byte{byte(i)}, i*300))
	}
	go c.Write(stream)
	got := make([]byte, len(stream))
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, stream) || h.frames.Load() != 100 {
		t.Fatalf("frames %d, data equal %v", h.frames.Load(), bytes.Equal(got, stream))
	}
}

// TLS 1.2 is not handed to the kernel: it stays in user space and works the same.
func TestTLS12UserSpace(t *testing.T) {
	cert, pool := testCert(t)
	s, addr := serve(t, &apiEcho{}, WithRings(1), WithTLS(&tls.Config{Certificates: []tls.Certificate{cert}}))
	c, err := tls.Dial("tcp4", addr, &tls.Config{RootCAs: pool, ServerName: "localhost", MaxVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if v := c.ConnectionState().Version; v != tls.VersionTLS12 {
		t.Fatalf("negotiated %x", v)
	}
	msg := make([]byte, 200000)
	rand.Read(msg)
	go c.Write(msg)
	got := make([]byte, len(msg))
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("TLS 1.2 echo: %v", err)
	}
	if st := s.Stats(); st.KernelTLSConnections != 0 {
		t.Fatalf("TLS 1.2 used kernel TLS: %+v", st)
	}
}

// Client data sent right after the handshake (before the server enabled kernel TLS) must not be
// lost: the handshake reads stop at the end of the client's Finished record.
func TestTLSDataRightAfterHandshake(t *testing.T) {
	forEachKTLS(t, func(t *testing.T, ktls bool) {
		cert, pool := testCert(t)
		_, addr := serve(t, &apiEcho{}, WithRings(1), WithTLS(&tls.Config{Certificates: []tls.Certificate{cert}}), WithKTLS(ktls))
		for i := 0; i < 20; i++ {
			raw, err := net.Dial("tcp4", addr)
			if err != nil {
				t.Fatal(err)
			}
			c := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: "localhost"})
			c.Handshake()
			c.Write([]byte("first")) // immediately, together with the end of the handshake
			got := make([]byte, 5)
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(c, got); err != nil || string(got) != "first" {
				t.Fatalf("connection %d: got %q, %v", i, got, err)
			}
			c.Close()
		}
	})
}
