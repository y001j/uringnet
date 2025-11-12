package main

import (
	"fmt"
	"github.com/y001j/uringnet"
	socket "github.com/y001j/uringnet/sockets"
	"os"
	"runtime"
	"sync"
)

type testServer struct {
	uringnet.BuiltinEventEngine

	testloop *uringnet.Ringloop
	//ring      *uring_net.URingNet
	addr      string
	multicore bool
}

// OnTraffic
//
//	@Description:
//	@receiver ts
//	@param data
//	@return uring_net.Action
func (ts *testServer) OnTraffic(data *uringnet.UserData, ringnet *uringnet.URingNet) uringnet.Action {
	data.WriteBuf = data.Buffer
	return uringnet.Echo
}

func (ts *testServer) OnWritten(data uringnet.UserData) uringnet.Action {
	//buf, _ := c.Next(-1)
	//thebuffer := ts.testloop.GetBuffer()
	//fmt.Println("Send Message to Client: \n", string(data))
	//fmt.Println("Send Message to Client: \n", uring_net.BytesToString(thebuffer[data.R1.(uint64)][:]))
	//c.Write(buf)

	return uringnet.None
}

func (ts *testServer) OnOpen(data *uringnet.UserData) ([]byte, uringnet.Action) {
	//buf, _ := c.Next(-1)
	//thebuffer := ts.testloop.GetBuffer()
	//fmt.Println("Send Message to Client: \n", string(data))
	//fmt.Println("Send Message to Client: \n", uring_net.BytesToString(thebuffer[data.R1.(uint64)][:]))
	//c.Write(buf)
	//c.SetContext(&httpCodec{parser: wildcat.NewHTTPParser()})
	//return nil, gnet.None
	return nil, uringnet.None
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: echoserver <address>")
		fmt.Println("Example: echoserver :8080")
		os.Exit(1)
	}

	addr := os.Args[1]

	options := socket.SocketOptions{TCPNoDelay: socket.TCPNoDelay, ReusePort: true}
	ringNets, err := uringnet.NewMany(uringnet.NetAddress{socket.Tcp4, addr}, 3200, true, runtime.NumCPU()*2-2, options, &testServer{})
	if err != nil {
		fmt.Printf("Failed to create uring instances: %v\n", err)
		os.Exit(1)
	}

	loop, err := uringnet.SetLoops(ringNets, 3000)
	if err != nil {
		fmt.Printf("Failed to set loops: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Echo server started on %s with %d rings\n", addr, runtime.NumCPU()*2-2)

	var waitgroup sync.WaitGroup
	waitgroup.Add(1)

	loop.RunMany()

	waitgroup.Wait()
}
