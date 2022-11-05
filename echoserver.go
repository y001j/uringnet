package main

import (
	uring_net "UringNet/uring-net"
	"os"
	"sync"
)

type testServer struct {
	uring_net.BuiltinEventEngine

	testloop *uring_net.Ringloop
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
func (ts *testServer) OnTraffic(data *uring_net.UserData, ringnet uring_net.URingNet) uring_net.Action {
	data.WriteBuf = data.Buffer
	return uring_net.Echo
}

func (ts *testServer) OnWritten(data uring_net.UserData) uring_net.Action {
	//buf, _ := c.Next(-1)
	//thebuffer := ts.testloop.GetBuffer()
	//fmt.Println("Send Message to Client: \n", string(data))
	//fmt.Println("Send Message to Client: \n", uring_net.BytesToString(thebuffer[data.R1.(uint64)][:]))
	//c.Write(buf)

	return uring_net.None
}

func (ts *testServer) OnOpen(data *uring_net.UserData) ([]byte, uring_net.Action) {
	//buf, _ := c.Next(-1)
	//thebuffer := ts.testloop.GetBuffer()
	//fmt.Println("Send Message to Client: \n", string(data))
	//fmt.Println("Send Message to Client: \n", uring_net.BytesToString(thebuffer[data.R1.(uint64)][:]))
	//c.Write(buf)
	//c.SetContext(&httpCodec{parser: wildcat.NewHTTPParser()})
	//return nil, gnet.None
	return nil, uring_net.None
}

func main() {
	addr := os.Args[1]

	//accptRingNet, _ := uring_net.New(uring_net.NetAddress{uring_net.TcpAddr, addr}, 500, true)

	//server := &testServer{}
	ringNets, _ := uring_net.NewMany(uring_net.NetAddress{uring_net.TcpAddr, addr}, 6000, true, 7, &testServer{}) //runtime.NumCPU()

	loop := uring_net.SetLoops(ringNets)

	//server.testloop = loop

	//loop := uring_net.SetLoop(ringNet)

	var waitgroup sync.WaitGroup
	waitgroup.Add(1)

	//RunMany(ringNets)
	//ringNets[0].Run2()

	//ringNets[0].EchoLoop()
	//go ringNets[0].RunAccept(ringNets)
	//loop.RunManyRW()
	loop.RunMany()

	waitgroup.Wait()
}
