package main

import (
	"net"
	"os"
)

// 标准库 echo，显式 Read/Write（数据进出用户态），避免 io.Copy 走 splice
func main() {
	l, err := net.Listen("tcp4", os.Args[1])
	if err != nil {
		panic(err)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 16384)
			for {
				n, err := c.Read(buf)
				if n > 0 {
					if _, err := c.Write(buf[:n]); err != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}(c)
	}
}
