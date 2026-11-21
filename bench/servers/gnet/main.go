package main

import (
	"os"

	"github.com/panjf2000/gnet/v2"
)

type echo struct{ gnet.BuiltinEventEngine }

func (e *echo) OnTraffic(c gnet.Conn) gnet.Action {
	buf, _ := c.Next(-1)
	c.Write(buf)
	return gnet.None
}

func main() {
	panic(gnet.Run(&echo{}, "tcp://"+os.Args[1], gnet.WithMulticore(true), gnet.WithReusePort(true), gnet.WithTCPNoDelay(gnet.TCPNoDelay)))
}
