//go:build linux
// +build linux

package modbus

import (
	"sync/atomic"

	"github.com/y001j/uringnet"
)

// Server is a Modbus slave: it answers requests with a Handler. Serve it with
// uringnet.Listen("tcp", ":502", s.Handler()).
//
// With RTU over TCP, frames with a wrong CRC are ignored (as on a serial line), unit ID 0 is a
// broadcast (writes are performed, nothing is sent back), and bytes that cannot start a frame
// (such as an unsupported function code, whose length is unknown) are discarded.
type Server struct {
	h       Handler
	framing Framing

	requests, dropped atomic.Uint64
}

// NewServer creates a server answering with h, using the given framing.
func NewServer(h Handler, framing Framing) *Server { return &Server{h: h, framing: framing} }

// Handler returns the uringnet handler serving Modbus requests.
func (s *Server) Handler() uringnet.Handler { return &serverHandler{s: s} }

// ServerStats are a Server's counters.
type ServerStats struct {
	Requests uint64 // requests executed
	Dropped  uint64 // frames dropped: wrong CRC, unparseable bytes
}

// Stats returns the server's counters.
func (s *Server) Stats() ServerStats {
	return ServerStats{Requests: s.requests.Load(), Dropped: s.dropped.Load()}
}

type serverHandler struct {
	uringnet.BuiltinHandler
	s *Server
}

func (h *serverHandler) OnTraffic(c *uringnet.Conn, _ []byte) uringnet.Action {
	s := h.s
	rtu := s.framing == RTUOverTCP
	var out []byte
	for {
		buf, _ := c.Peek(-1)
		if len(buf) == 0 {
			break
		}
		var n int
		if rtu {
			n = rtuFrameLen(buf, true)
		} else {
			n = tcpFrameLen(buf)
		}
		if n == 0 {
			break
		}
		if n < 0 {
			s.dropped.Add(1)
			if !rtu {
				if len(out) > 0 {
					c.Write(out)
				}
				return uringnet.Close
			}
			c.Discard(-1)
			break
		}
		frame := buf[:n]
		if rtu {
			if !checkCRC(frame) {
				s.dropped.Add(1)
			} else {
				s.requests.Add(1)
				unit := frame[0]
				if resp := serve(s.h, unit, frame[1:n-2]); resp != nil && unit != 0 {
					out = appendRTU(out, unit, resp)
				}
			}
		} else {
			s.requests.Add(1)
			if resp := serve(s.h, frame[6], frame[mbapLen:]); resp != nil {
				out = appendMBAP(out, uint16(frame[0])<<8|uint16(frame[1]), frame[6], resp)
			}
		}
		c.Discard(n)
	}
	if len(out) > 0 {
		c.Write(out)
	}
	return uringnet.None
}
