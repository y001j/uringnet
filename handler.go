//go:build linux
// +build linux

package uringnet

// connHandler is the internal interface the connection state machine calls back through. The deprecated
// API (EventHandler + UserData) and the Handler API (Handler + Conn) each implement it over the same state machine. All methods run on the loop thread.
//
// open and traffic run while c.batching is set: data written during them is sent together after they return.
type connHandler interface {
	boot(rn *URingNet)
	open(c *conn)
	traffic(c *conn, data []byte)
	written(c *conn)
	closed(c *conn)
	shutdown(rn *URingNet)
}

// legacyHandler connects the deprecated EventHandler to the connection state machine.
type legacyHandler struct {
	h EventHandler
}

func (l legacyHandler) boot(rn *URingNet) { l.h.OnBoot(rn) }

func (l legacyHandler) open(c *conn) {
	out, action := l.h.OnOpen(&c.ud)
	if len(out) > 0 {
		c.write(out)
	}
	c.legacyAction(action)
}

func (l legacyHandler) traffic(c *conn, data []byte) {
	c.ud.Buffer = data
	c.ud.BufSize = int32(len(data))
	action := l.h.OnTraffic(&c.ud, c.rn)
	c.legacyAction(action)
	c.ud.Buffer = nil
	c.ud.BufSize = 0
}

func (l legacyHandler) written(c *conn) {
	if l.h.OnWritten(c.ud) == Close {
		c.close()
	}
}

func (l legacyHandler) closed(c *conn) { l.h.OnClose(c.ud) }

func (l legacyHandler) shutdown(rn *URingNet) { l.h.OnShutdown(rn) }

// legacyAction handles the action returned by a deprecated callback: sends UserData.WriteBuf and so on.
func (c *conn) legacyAction(action Action) {
	switch action {
	case Echo, Write:
		c.send(c.ud.WriteBuf)
	case EchoAndClose:
		c.send(c.ud.WriteBuf)
		c.closeAfterFlush()
	case Close:
		c.close()
	case Shutdown:
		if rl := c.rn.ringloop; rl != nil {
			go rl.Shutdown()
		}
	}
	// Reset WriteBuf so its backing array can be reused (e.g. appending a response); but if it points
	// into the receive buffer (data.WriteBuf = data.Buffer) it must be dropped, or the next append
	// would write into a buffer that belongs to the kernel again.
	if overlaps(c.ud.WriteBuf, c.curBuf) {
		c.ud.WriteBuf = nil
	} else {
		c.ud.WriteBuf = c.ud.WriteBuf[:0]
	}
}
