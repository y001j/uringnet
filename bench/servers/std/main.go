// Standard library echo: io.Copy(c, c) uses splice on Linux (zero-copy forwarding in the kernel, data never reaches
// user space), so it does not represent servers that process data; use stdcopy for a fair comparison.

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
