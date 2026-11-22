//go:build linux
// +build linux

package uringnet

// connHandler 是连接状态机调用回调的内部接口。旧 API（EventHandler + UserData）和新 API
// （Handler + Conn）各有一个实现，共用同一套连接状态机。所有方法都在 Loop 线程中调用。
//
// open 与 traffic 在 c.batching 期间调用：其间写入的数据在回调返回后一起发送。
type connHandler interface {
	boot(rn *URingNet)
	open(c *conn)
	traffic(c *conn, data []byte)
	written(c *conn)
	closed(c *conn)
	shutdown(rn *URingNet)
}

// legacyHandler 把旧的 EventHandler 接到连接状态机上
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

// legacyAction 处理旧 API 回调返回的动作：发送 UserData.WriteBuf 等
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
	// 重置 WriteBuf 以便用户复用其底层数组（如 append 构造响应）；
	// 但如果它指向接收缓冲区（data.WriteBuf = data.Buffer），必须丢弃，
	// 否则用户下次 append 会写进已归还给内核的缓冲区。
	if overlaps(c.ud.WriteBuf, c.curBuf) {
		c.ud.WriteBuf = nil
	} else {
		c.ud.WriteBuf = c.ud.WriteBuf[:0]
	}
}
