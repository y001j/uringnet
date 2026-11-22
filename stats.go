//go:build linux
// +build linux

package uringnet

import "time"

// Stats are counters for a Server since it was created, summed over its rings.
// Publish them with expvar.Func or your metrics library of choice.
type Stats struct {
	Connections   int64  // connections open now
	Accepted      uint64 // connections accepted
	Dialed        uint64 // outgoing connections established (Dial, DialAsync)
	AcceptErrors  uint64 // failed accepts (for example EMFILE); retried with backoff
	BytesRead     uint64 // bytes received
	BytesWritten  uint64 // bytes sent
	IdleTimeouts  uint64 // connections closed by the idle timeout
	WriteTimeouts uint64 // connections closed by the write timeout

	// RecvBufferExhausted counts how often a connection's receiving stopped because every
	// receive buffer of its ring was in use (-ENOBUFS); receiving resumes once buffers are
	// returned. A steadily growing value means the ring needs more buffers (WithBuffers).
	RecvBufferExhausted uint64
	// BackpressurePauses counts how often a connection stopped receiving because its send
	// queue exceeded the limit (WithMaxWriteBuffer).
	BackpressurePauses uint64
	// ZeroCopySends counts sends made with SEND_ZC (WithZeroCopy).
	ZeroCopySends uint64

	// TLSHandshakeErrors counts connections closed because their TLS handshake failed.
	TLSHandshakeErrors uint64

	// UDP (PacketServer)
	PacketsReceived  uint64 // datagrams delivered to OnPacket
	PacketsSent      uint64 // datagrams sent
	PacketsTruncated uint64 // datagrams dropped because they did not fit in a receive buffer
	SendErrors       uint64 // failed datagram sends
}

// Stats returns the server's counters. It is safe to call from any goroutine: the counters
// are read on each event loop, so the call waits briefly for every loop to get to it.
// Rings that are already closed contribute nothing.
func (s *Server) Stats() Stats {
	results := make(chan Stats, len(s.rings))
	posted := 0
	for _, rn := range s.rings {
		rn := rn
		if rn.Post(func() { results <- rn.stats() }) == nil {
			posted++
		}
	}
	var total Stats
	timeout := time.After(time.Second)
	for i := 0; i < posted; i++ {
		select {
		case st := <-results:
			total.add(st)
		case <-timeout:
			return total
		}
	}
	return total
}

// stats reads this ring's counters; loop thread only.
func (rn *URingNet) stats() Stats {
	return Stats{
		Connections:         int64(len(rn.conns)),
		Accepted:            rn.accepted,
		Dialed:              rn.dialed,
		AcceptErrors:        rn.acceptErrs,
		BytesRead:           rn.bytesIn,
		BytesWritten:        rn.bytesOut,
		IdleTimeouts:        rn.idleCloses,
		WriteTimeouts:       rn.writeCloses,
		RecvBufferExhausted: rn.enobufs,
		BackpressurePauses:  uint64(rn.pauses),
		ZeroCopySends:       uint64(rn.zcSends),
		TLSHandshakeErrors:  rn.tlsErrors,
		PacketsReceived:     rn.packetsIn,
		PacketsSent:         rn.packetsOut,
		PacketsTruncated:    rn.truncated,
		SendErrors:          rn.sendErrs,
	}
}

func (t *Stats) add(s Stats) {
	t.Connections += s.Connections
	t.Accepted += s.Accepted
	t.Dialed += s.Dialed
	t.AcceptErrors += s.AcceptErrors
	t.BytesRead += s.BytesRead
	t.BytesWritten += s.BytesWritten
	t.IdleTimeouts += s.IdleTimeouts
	t.WriteTimeouts += s.WriteTimeouts
	t.RecvBufferExhausted += s.RecvBufferExhausted
	t.BackpressurePauses += s.BackpressurePauses
	t.ZeroCopySends += s.ZeroCopySends
	t.TLSHandshakeErrors += s.TLSHandshakeErrors
	t.PacketsReceived += s.PacketsReceived
	t.PacketsSent += s.PacketsSent
	t.PacketsTruncated += s.PacketsTruncated
	t.SendErrors += s.SendErrors
}
