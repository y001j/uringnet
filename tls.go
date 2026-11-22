//go:build linux
// +build linux

package uringnet

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// tlsHandshakeTimeout bounds the TLS handshake of a connection.
const tlsHandshakeTimeout = 10 * time.Second

// Kernel TLS constants from linux/tls.h that golang.org/x/sys does not define.
const (
	tlsTX                  = 1
	tlsRX                  = 2
	tlsRXExpectNoPad       = 4
	tls13Version           = 0x0304
	tlsCipherAESGCM128     = 51
	tlsCipherAESGCM256     = 52
	tlsCipherChaCha20Poly  = 54
	tlsSetRecordType       = 1
	tlsGetRecordType       = 2
	tlsRecordAlert         = 21
	tlsRecordHandshake     = 22
	tlsRecordApplication   = 23
	tlsHandshakeKeyUpdate  = 24
	tlsAlertLevelWarning   = 1
	tlsAlertCloseNotify    = 0
	tlsMaxRecordCiphertext = 16384 + 256 + 5
)

// ktlsUnavailable is set once the kernel has refused TCP_ULP "tls" (module not loaded), so later
// connections do not try again.
var ktlsUnavailable atomic.Bool

// tlsState is the TLS layer of one connection.
//
// The handshake runs on a goroutine of its own with crypto/tls on a blocking transport that reads
// the socket directly, one TLS record at a time, so nothing after the handshake is read from the
// socket. Then, for TLS 1.3 with a supported cipher, the traffic keys go to the kernel (kTLS): the
// kernel encrypts and decrypts during the copies it makes anyway, and the connection's data path is
// the same as for plain TCP (in-place sends included). Otherwise the connection continues with
// crypto/tls in user space on the loop thread: the transport turns non-blocking and returns a
// temporary net.Error when a record is incomplete, which crypto/tls tolerates after the handshake.
type tlsState struct {
	c       *conn
	tc      *tls.Conn
	tr      *tlsTransport
	keys    *keyLog
	client  bool
	onReady func(*conn, error) // dial: reports the result after the handshake; loop thread only

	// Loop thread only.
	ready       bool // handshake done and OnOpen called
	rxKernel    bool // the kernel decrypts received records
	txKernel    bool // the kernel encrypts sent data
	notifyClose bool // send a close_notify alert before shutting the socket down (kernel TX)
}

// startTLS starts the handshake of a new connection on a goroutine. The event loop does not touch
// the socket until the handshake is done.
func (c *conn) startTLS(cfg *tls.Config, client bool, onReady func(*conn, error)) {
	rn := c.rn
	tr := &tlsTransport{rn: rn, fd: -1}
	if sa, err := unix.Getsockname(c.fd); err == nil {
		tr.local = sockaddrToNetAddr(sa)
	}
	if sa, err := unix.Getpeername(c.fd); err == nil {
		tr.remote = sockaddrToNetAddr(sa)
	}
	ts := &tlsState{c: c, tr: tr, client: client, onReady: onReady}
	tr.flush = func() { ts.flushOut() }

	// A private copy of the config per connection: its KeyLogWriter captures this connection's
	// traffic secrets for kTLS (and still forwards to the user's writer, if any)
	cfg = cfg.Clone()
	if rn.ktls && !ktlsUnavailable.Load() {
		ts.keys = &keyLog{next: cfg.KeyLogWriter}
		cfg.KeyLogWriter = ts.keys
		if !client {
			// Go servers send session tickets protected with the application keys in the same flight
			// as their Finished, so the number of records already sent under those keys, which the
			// kernel needs, would be unknown. Without tickets it is zero.
			cfg.SessionTicketsDisabled = true
		}
	}
	if client {
		ts.tc = tls.Client(tr, cfg)
	} else {
		ts.tc = tls.Server(tr, cfg)
	}
	c.tls = ts

	// The goroutine uses its own descriptor for the socket: if the connection is closed meanwhile,
	// the loop may close and reuse the original fd number, while the shutdown it does first still
	// wakes the handshake.
	fd, err := unix.Dup(c.fd)
	if err != nil {
		go rn.Post(func() { ts.handshakeDone(err, false, false) })
		return
	}
	unix.CloseOnExec(fd)
	unix.SetNonblock(fd, false) // the epoll backend makes it non-blocking again when it registers the fd
	tr.fd = fd
	go ts.run()
}

// run is the connection's handshake goroutine.
func (ts *tlsState) run() {
	ctx, cancel := context.WithTimeout(context.Background(), tlsHandshakeTimeout)
	err := ts.tc.HandshakeContext(ctx)
	cancel()
	var rx, tx bool
	if err == nil {
		rx, tx = ts.setupKTLS()
	}
	ts.tr.detach()
	ts.c.rn.Post(func() { ts.handshakeDone(err, rx, tx) })
}

// setupKTLS hands the traffic keys to the kernel, if possible. It runs on the handshake goroutine,
// before the event loop reads the socket. A failure is not an error: the connection then uses
// crypto/tls in user space.
func (ts *tlsState) setupKTLS() (rx, tx bool) {
	if ts.keys == nil {
		return false, false
	}
	state := ts.tc.ConnectionState()
	if state.Version != tls.VersionTLS13 {
		return false, false
	}
	clientSecret, serverSecret := ts.keys.secrets()
	if clientSecret == nil || serverSecret == nil {
		return false, false
	}
	txSecret, rxSecret := serverSecret, clientSecret
	if ts.client {
		txSecret, rxSecret = clientSecret, serverSecret
	}
	rxInfo, err := ktlsCryptoInfo(state.CipherSuite, rxSecret)
	if err != nil {
		return false, false
	}
	txInfo, err := ktlsCryptoInfo(state.CipherSuite, txSecret)
	if err != nil {
		return false, false
	}
	fd := ts.tr.fd
	if err := unix.SetsockoptString(fd, unix.IPPROTO_TCP, unix.TCP_ULP, "tls"); err != nil {
		if err == unix.ENOENT && ktlsUnavailable.CompareAndSwap(false, true) {
			// The tls module is not loaded (processes without CAP_NET_ADMIN cannot load it)
			ts.c.rn.log.Info("uringnet: kernel TLS unavailable (run \"modprobe tls\"), using crypto/tls in user space")
		}
		return false, false
	}
	// RX first: if the kernel cannot decrypt, it does not encrypt either, and user space keeps both
	if unix.SetsockoptString(fd, unix.SOL_TLS, tlsRX, string(rxInfo)) != nil {
		return false, false
	}
	// TLS 1.3 records end with padding and the real content type, so by default the kernel decrypts
	// into its own buffer and copies. Promising no padding (Linux 6.0+) lets it decrypt straight into
	// the receive buffer; a padded record from the peer still works, through a slower retry.
	unix.SetsockoptInt(fd, unix.SOL_TLS, tlsRXExpectNoPad, 1)
	tx = unix.SetsockoptString(fd, unix.SOL_TLS, tlsTX, string(txInfo)) == nil
	return true, tx
}

// handshakeDone runs on the loop thread when the handshake finished or failed.
func (ts *tlsState) handshakeDone(err error, rx, tx bool) {
	c := ts.c
	onReady := ts.onReady
	ts.onReady = nil
	if err == nil && c.closed {
		err = ErrConnClosed
	}
	if err != nil {
		c.rn.tlsErrors++
		c.rn.log.Debug("uringnet: TLS handshake failed", "fd", c.fd, "err", err)
		c.fail(err) // OnOpen was never called, so OnClose is not either
		if onReady != nil {
			onReady(nil, err)
		}
		return
	}
	ts.ready = true
	ts.rxKernel, ts.txKernel = rx, tx
	if rx {
		c.rn.ktlsConns++
	}
	c.lastActive, c.lastSend = c.rn.tick, c.rn.tick
	c.callOpen()
	if !c.closed && !c.paused {
		c.armRecv()
	}
	if onReady != nil {
		onReady(c, nil)
	}
}

// pump decrypts the records received so far in user space and passes them to OnTraffic (loop
// thread, after the handshake, without kernel RX). It stops when the next record is incomplete,
// the connection ends or the peer closed.
func (ts *tlsState) pump() {
	c := ts.c
	if c.rn.tlsBuf == nil {
		c.rn.tlsBuf = make([]byte, 64<<10) // shared by the ring's connections: data is valid during OnTraffic only
	}
	for !c.closed {
		n, err := ts.tc.Read(c.rn.tlsBuf)
		if n > 0 {
			c.deliverPlain(c.rn.tlsBuf[:n])
		}
		if err != nil {
			if err != errWouldBlock {
				ts.readDone(err)
			}
			return
		}
	}
}

// readDone handles the end of the read side: the peer closed (io.EOF) or an error.
func (ts *tlsState) readDone(err error) {
	c := ts.c
	if c.closed {
		return
	}
	if err == io.EOF {
		// All data before the peer's close_notify (or EOF) has been delivered: send what was
		// written, then close, as for a plain half-close
		if c.err == nil {
			c.err = io.EOF
		}
		c.closeGracefully()
		return
	}
	c.fail(err)
}

// write encrypts b in user space on the loop thread and queues the ciphertext (no kernel TX).
func (ts *tlsState) write(b []byte) {
	if len(b) == 0 {
		return
	}
	if _, err := ts.tc.Write(b); err != nil {
		ts.c.fail(err)
		return
	}
	ts.flushOut()
}

// closeNotify arranges for a close_notify alert (loop thread).
func (ts *tlsState) closeNotify() {
	if !ts.ready {
		return
	}
	if ts.txKernel {
		ts.notifyClose = true // sent by conn.close, once the queued data is out
		return
	}
	ts.tc.CloseWrite()
	ts.flushOut()
}

// sendCloseNotify sends a close_notify alert through kernel TLS (loop thread, nothing in flight).
func (ts *tlsState) sendCloseNotify() {
	oob := make([]byte, unix.CmsgSpace(1))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&oob[0]))
	h.Level = unix.SOL_TLS
	h.Type = tlsSetRecordType
	h.SetLen(unix.CmsgLen(1))
	oob[unix.CmsgLen(0)] = tlsRecordAlert
	unix.SendmsgN(ts.c.fd, []byte{tlsAlertLevelWarning, tlsAlertCloseNotify}, oob, nil, unix.MSG_DONTWAIT|unix.MSG_NOSIGNAL)
}

// controlRecord reads and handles the non-data record that made a kernel-TLS receive fail with EIO
// (loop thread). It reports whether receiving should continue.
func (ts *tlsState) controlRecord() bool {
	c := ts.c
	buf := make([]byte, tlsMaxRecordCiphertext)
	oob := make([]byte, unix.CmsgSpace(1))
	n, oobn, _, _, err := unix.Recvmsg(c.fd, buf, oob, unix.MSG_DONTWAIT)
	if err != nil {
		if err == unix.EAGAIN {
			return true
		}
		c.fail(err)
		return false
	}
	if n == 0 {
		ts.readDone(io.EOF)
		return false
	}
	typ := byte(tlsRecordApplication)
	if msgs, err := unix.ParseSocketControlMessage(oob[:oobn]); err == nil {
		for _, m := range msgs {
			if m.Header.Level == unix.SOL_TLS && m.Header.Type == tlsGetRecordType && len(m.Data) > 0 {
				typ = m.Data[0]
			}
		}
	}
	data := buf[:n]
	switch typ {
	case tlsRecordApplication: // data after all
		c.lastActive = c.rn.tick
		c.rn.bytesIn += uint64(n)
		c.deliverPlain(data)
	case tlsRecordAlert:
		if len(data) >= 2 && data[1] == tlsAlertCloseNotify {
			ts.readDone(io.EOF)
			return false
		}
		c.fail(fmt.Errorf("uringnet: TLS alert %v from the peer", data))
		return false
	case tlsRecordHandshake:
		if len(data) > 0 && data[0] == tlsHandshakeKeyUpdate {
			c.fail(errors.New("uringnet: TLS KeyUpdate is not supported with kernel TLS"))
			return false
		}
		// NewSessionTicket and other post-handshake messages: ignored
	default:
		c.fail(fmt.Errorf("uringnet: unexpected TLS record type %d", typ))
		return false
	}
	return !c.closed && !c.closeAfter
}

// flushOut moves the ciphertext written by tls.Conn into the send queue (loop thread).
func (ts *tlsState) flushOut() {
	tr := ts.tr
	c := ts.c
	// Give the transport a copy buffer the connection has finished sending, so encrypting the next
	// write does not allocate
	var next []byte
	if n := len(c.spare); n > 0 {
		next = c.spare[n-1][:0]
		c.spare = c.spare[:n-1]
	}
	tr.mu.Lock()
	out := tr.out
	if len(out) > 0 {
		tr.out = next
		next = nil
	}
	tr.flushQueued = false
	tr.mu.Unlock()
	if next != nil {
		c.spare = append(c.spare, next)
	}
	if len(out) > 0 && !ts.c.closed {
		ts.c.writeOwned(out) // the transport starts a new buffer, so this one can be queued without a copy
	}
}

// deliverPlain passes decrypted data to OnTraffic (loop thread). Inside onRecv the caller sends
// what was written; on its own it sends it here.
func (c *conn) deliverPlain(data []byte) {
	if c.closed || c.closeAfter {
		return
	}
	outer := c.batching
	c.batching = true
	t0 := c.rn.cbBegin()
	c.rn.h.traffic(c, data)
	c.rn.cbEnd(t0, "OnTraffic", c.fd)
	c.batching = outer
	if !outer {
		c.flush()
		c.checkBackpressure()
	}
}

// errWouldBlock is returned by the transport after the handshake when no complete record is
// buffered. It is a temporary net.Error, which crypto/tls does not record as a fatal error.
var errWouldBlock error = wouldBlock{}

type wouldBlock struct{}

func (wouldBlock) Error() string   { return "uringnet: TLS record incomplete" }
func (wouldBlock) Timeout() bool   { return true }
func (wouldBlock) Temporary() bool { return true }

// tlsTransport is the net.Conn under tls.Conn. During the handshake it reads and writes the socket
// directly (fd >= 0), never reading past the end of a record. Afterwards, in user-space mode, the
// event loop lends it received ciphertext and collects what tls.Conn writes.
type tlsTransport struct {
	rn *URingNet

	// Handshake mode, used by the handshake goroutine only
	fd        int // dup of the socket, -1 after the handshake
	recRemain int // bytes left in the current record

	mu          sync.Mutex
	in          []byte // ciphertext available to tls.Conn
	inErr       error  // io.EOF after the peer closed, net.ErrClosed after Close
	out         []byte // ciphertext written by tls.Conn and not yet queued
	flushQueued bool
	closed      bool
	flush       func()
	local       net.Addr
	remote      net.Addr
}

// detach ends handshake mode and closes the transport's own descriptor (handshake goroutine).
func (t *tlsTransport) detach() {
	t.mu.Lock()
	fd := t.fd
	t.fd = -1
	t.mu.Unlock()
	if fd >= 0 {
		unix.Close(fd)
	}
}

// lend makes b readable without copying it; unlend must follow before b is reused (loop thread).
func (t *tlsTransport) lend(b []byte) {
	t.mu.Lock()
	if len(t.in) == 0 {
		t.in = b
	} else {
		t.in = append(t.in, b...)
	}
	t.mu.Unlock()
}

// unlend copies whatever crypto/tls left unread, so the lent buffer can be reused.
func (t *tlsTransport) unlend() {
	t.mu.Lock()
	if len(t.in) > 0 {
		t.in = append([]byte(nil), t.in...)
	} else {
		t.in = nil
	}
	t.mu.Unlock()
}

// end makes Read return err once the buffered data is consumed.
func (t *tlsTransport) end(err error) {
	t.mu.Lock()
	if t.inErr == nil {
		t.inErr = err
	}
	t.mu.Unlock()
}

func (t *tlsTransport) handshaking() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fd >= 0
}

func (t *tlsTransport) Read(p []byte) (int, error) {
	if t.handshaking() {
		return t.readRecord(p)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.in) > 0 {
		n := copy(p, t.in)
		t.in = t.in[n:]
		return n, nil
	}
	if t.inErr != nil {
		return 0, t.inErr
	}
	return 0, errWouldBlock
}

// readRecord reads the socket without going past the end of the current TLS record, so that what
// follows the handshake stays in the socket for the kernel or the event loop.
func (t *tlsTransport) readRecord(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if t.recRemain == 0 {
		if len(p) < 5 {
			return 0, io.ErrShortBuffer
		}
		if err := readFull(t.fd, p[:5]); err != nil {
			return 0, err
		}
		t.recRemain = int(p[3])<<8 | int(p[4])
		return 5, nil
	}
	n := len(p)
	if n > t.recRemain {
		n = t.recRemain
	}
	for {
		m, err := unix.Read(t.fd, p[:n])
		switch {
		case err == unix.EINTR:
			continue
		case err != nil:
			return 0, err
		case m == 0:
			return 0, io.ErrUnexpectedEOF
		}
		t.recRemain -= m
		return m, nil
	}
}

func readFull(fd int, p []byte) error {
	for len(p) > 0 {
		n, err := unix.Read(fd, p)
		switch {
		case err == unix.EINTR:
			continue
		case err != nil:
			return err
		case n == 0:
			return io.EOF
		}
		p = p[n:]
	}
	return nil
}

func (t *tlsTransport) Write(p []byte) (int, error) {
	if t.handshaking() {
		// Handshake: write the socket directly (blocking)
		for done := 0; done < len(p); {
			n, err := unix.SendmsgN(t.fd, p[done:], nil, nil, unix.MSG_NOSIGNAL)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				return done, err
			}
			done += n
		}
		return len(p), nil
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return 0, net.ErrClosed
	}
	t.out = append(t.out, p...)
	post := !t.flushQueued
	t.flushQueued = true
	t.mu.Unlock()
	if post {
		// Records written outside a Write on the loop thread (e.g. an alert answering the peer during
		// pump) are sent by a posted flush; writes on the loop thread flush right away
		t.rn.Post(t.flush)
	}
	return len(p), nil
}

// Close is called by crypto/tls when the handshake times out or fails: shut the socket down so a
// blocked read returns.
func (t *tlsTransport) Close() error {
	t.mu.Lock()
	fd := t.fd
	t.closed = true
	if t.inErr == nil {
		t.inErr = net.ErrClosed
	}
	t.mu.Unlock()
	if fd >= 0 {
		unix.Shutdown(fd, unix.SHUT_RDWR)
	}
	return nil
}

func (t *tlsTransport) LocalAddr() net.Addr              { return t.local }
func (t *tlsTransport) RemoteAddr() net.Addr             { return t.remote }
func (t *tlsTransport) SetDeadline(time.Time) error      { return nil }
func (t *tlsTransport) SetReadDeadline(time.Time) error  { return nil }
func (t *tlsTransport) SetWriteDeadline(time.Time) error { return nil }

// keyLog captures the TLS 1.3 traffic secrets of one connection from crypto/tls's key log.
type keyLog struct {
	next io.Writer // the user's KeyLogWriter, if any

	mu             sync.Mutex
	client, server []byte
}

func (k *keyLog) Write(line []byte) (int, error) {
	if f := strings.Fields(string(line)); len(f) == 3 {
		if secret, err := hex.DecodeString(f[2]); err == nil {
			k.mu.Lock()
			switch f[0] {
			case "CLIENT_TRAFFIC_SECRET_0":
				k.client = secret
			case "SERVER_TRAFFIC_SECRET_0":
				k.server = secret
			}
			k.mu.Unlock()
		}
	}
	if k.next != nil {
		return k.next.Write(line)
	}
	return len(line), nil
}

func (k *keyLog) secrets() (client, server []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.client, k.server
}

// ktlsCryptoInfo builds the kernel's struct tls12_crypto_info_* for a TLS 1.3 traffic secret, with
// record sequence number 0.
func ktlsCryptoInfo(suite uint16, secret []byte) ([]byte, error) {
	var (
		cipher uint16
		keyLen int
		h      func() hash.Hash
	)
	switch suite {
	case tls.TLS_AES_128_GCM_SHA256:
		cipher, keyLen, h = tlsCipherAESGCM128, 16, sha256.New
	case tls.TLS_AES_256_GCM_SHA384:
		cipher, keyLen, h = tlsCipherAESGCM256, 32, sha512.New384
	case tls.TLS_CHACHA20_POLY1305_SHA256:
		cipher, keyLen, h = tlsCipherChaCha20Poly, 32, sha256.New
	default:
		return nil, fmt.Errorf("uringnet: cipher suite %#x not supported by kernel TLS", suite)
	}
	key := hkdfExpandLabel(h, secret, "key", keyLen)
	iv := hkdfExpandLabel(h, secret, "iv", 12)
	// struct tls_crypto_info: version and cipher_type as native-endian u16
	info := make([]byte, 4, 64)
	*(*uint16)(unsafe.Pointer(&info[0])) = tls13Version
	*(*uint16)(unsafe.Pointer(&info[2])) = cipher
	if cipher == tlsCipherChaCha20Poly {
		// iv[12] key[32] rec_seq[8]
		info = append(info, iv...)
		info = append(info, key...)
	} else {
		// iv[8] key[16|32] salt[4] rec_seq[8]: the 12-byte TLS 1.3 IV is salt (first 4 bytes) + iv
		info = append(info, iv[4:]...)
		info = append(info, key...)
		info = append(info, iv[:4]...)
	}
	info = append(info, make([]byte, 8)...) // rec_seq
	return info, nil
}

// hkdfExpandLabel is HKDF-Expand-Label from RFC 8446 with an empty context.
func hkdfExpandLabel(h func() hash.Hash, secret []byte, label string, length int) []byte {
	full := "tls13 " + label
	info := []byte{byte(length >> 8), byte(length), byte(len(full))}
	info = append(info, full...)
	info = append(info, 0) // empty context
	var out, t []byte
	for i := byte(1); len(out) < length; i++ {
		m := hmac.New(h, secret)
		m.Write(t)
		m.Write(info)
		m.Write([]byte{i})
		t = m.Sum(nil)
		out = append(out, t...)
	}
	return out[:length]
}
