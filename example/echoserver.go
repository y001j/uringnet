package main

import (
	"github.com/y001j/uringnet"
	socket "github.com/y001j/uringnet/sockets"
	"os"
	"runtime"
	"sync"
)

type testServer struct {
	UringNet.BuiltinEventEngine

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

func (ts *testServer) OnWritten(data uringnet.UserData) UringNet.Action {
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
	addr := os.Args[1]

	//accptRingNet, _ := uring_net.New(uring_net.NetAddress{uring_net., addr}, 500, true)
	//TcpAddr
	options := socket.SocketOptions{TCPNoDelay: socket.TCPNoDelay, ReusePort: true}
	ringNets, _ := uringnet.NewMany(uringnet.NetAddress{socket.Tcp4, addr}, 3200, true, runtime.NumCPU()*2-2, options, &testServer{}) //runtime.NumCPU()

	loop := uringnet.SetLoops(ringNets, 3000)
	var waitgroup sync.WaitGroup
	waitgroup.Add(1)

	loop.RunMany()

	waitgroup.Wait()
}
