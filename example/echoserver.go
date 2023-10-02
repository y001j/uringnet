package main

import (
	"github.com/y001j/UringNet"
	socket "github.com/y001j/UringNet/sockets"
	"os"
	"runtime"
	"sync"
)

type testServer struct {
	UringNet.BuiltinEventEngine

	testloop *UringNet.Ringloop
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
func (ts *testServer) OnTraffic(data *UringNet.UserData, ringnet UringNet.URingNet) UringNet.Action {
	data.WriteBuf = data.Buffer
	return UringNet.Echo
}

func (ts *testServer) OnWritten(data UringNet.UserData) UringNet.Action {
	//buf, _ := c.Next(-1)
	//thebuffer := ts.testloop.GetBuffer()
	//fmt.Println("Send Message to Client: \n", string(data))
	//fmt.Println("Send Message to Client: \n", uring_net.BytesToString(thebuffer[data.R1.(uint64)][:]))
	//c.Write(buf)

	return UringNet.None
}

func (ts *testServer) OnOpen(data *UringNet.UserData) ([]byte, UringNet.Action) {
	//buf, _ := c.Next(-1)
	//thebuffer := ts.testloop.GetBuffer()
	//fmt.Println("Send Message to Client: \n", string(data))
	//fmt.Println("Send Message to Client: \n", uring_net.BytesToString(thebuffer[data.R1.(uint64)][:]))
	//c.Write(buf)
	//c.SetContext(&httpCodec{parser: wildcat.NewHTTPParser()})
	//return nil, gnet.None
	return nil, UringNet.None
}

func main() {
	addr := os.Args[1]

	//accptRingNet, _ := uring_net.New(uring_net.NetAddress{uring_net., addr}, 500, true)
	//TcpAddr
	options := socket.SocketOptions{TCPNoDelay: socket.TCPNoDelay, ReusePort: true}
	ringNets, _ := UringNet.NewMany(UringNet.NetAddress{socket.Tcp4, addr}, 3200, true, runtime.NumCPU()*2-2, options, &testServer{}) //runtime.NumCPU()

	loop := UringNet.SetLoops(ringNets, 3000)
	var waitgroup sync.WaitGroup
	waitgroup.Add(1)

	loop.RunMany()

	waitgroup.Wait()
}
