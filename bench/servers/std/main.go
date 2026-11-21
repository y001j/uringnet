// 标准库 echo：io.Copy(c, c) 在 Linux 上会走 splice（内核内零拷贝转发，数据不进用户态），
// 不能代表需要处理数据的服务；公平对比请用 stdcopy。

package main

import (
	"io"
	"net"
	"os"
)

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
		go func() { buf := make([]byte, 16384); io.CopyBuffer(c, c, buf); c.Close() }()
	}
}
