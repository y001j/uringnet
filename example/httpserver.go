package main

import (
	"UringNet"
	"bytes"
	"github.com/evanphx/wildcat"
	"log"
	"os"
	"sync"
	"time"
)

type testServer struct {
	UringNet.BuiltinEventEngine

	testloop *UringNet.Ringloop
	//ring      *uring_net.URingNet
	addr      string
	multicore bool
}

type httpCodec struct {
	parser *wildcat.HTTPParser
	buf    []byte
}

func (hc *httpCodec) appendResponse() {
	hc.buf = append(hc.buf, "HTTP/1.1 200 OK\r\nServer: gnet\r\nContent-Type: text/plain\r\nDate: Mon, 02 Jan 2022 15:04:05 GMT"...)
	//hc.buf = time.Now().AppendFormat(hc.buf, "Mon, 02 Jan 2006 15:04:05 GMT")
	hc.buf = append(hc.buf, "\r\nContent-Length: 12\r\n\r\nHello World!"...)
}

func appendResponse(hc *[]byte) {
	*hc = append(*hc, "HTTP/1.1 200 OK\r\nServer: uringNet\r\nContent-Type: text/plain\r\nDate: "...)
	*hc = time.Now().AppendFormat(*hc, "Mon, 02 Jan 2006 15:04:05 GMT")
	*hc = append(*hc, "\r\nContent-Length: 12\r\n\r\nHello World!"...)
}

var (
	errMsg      = "Internal Server Error"
	errMsgBytes = []byte(errMsg)
)

// OnTraffic
//
//	@Description:
//	@receiver ts
//	@param data
//	@return uring_net.Action
func (ts *testServer) OnTraffic(data *UringNet.UserData, ringnet UringNet.URingNet) UringNet.Action {

	//hc := ts.Context().(*httpCodec)

	parser := ts.Context().(*httpCodec).parser
	//data.WriteBuf = make([]byte, 2048)

pipeline:
	//data.Buffer = data.Buffer[:data.BufSize]ringNet.Autobuffer[offset]
	buffer := data.Buffer[:data.BufSize] //ringnet.ReadBuffer[:data.BufSize]
	//log.Println("data:", " offset: ", uring_net.BytesToString(buffer), " ", data.BufOffset)
	headerOffset, err := parser.Parse(buffer)
	//fmt.Println("data:", "head offset: ", headerOffset) //uring_net.BytesToString(data.Buffer)) // "head offset: ", headerOffset)
	//fmt.Println("data:", "head offset: ", headerOffset)
	//time.Sleep(time.Millisecond * 15)
	if err != nil {
		//c.Write(errMsgBytes)
		//fmt.Println("error message: ", err.Error())

		return UringNet.Close
	}

	appendResponse(&data.WriteBuf)

	//hc.appendResponse()
	bodyLen := int(parser.ContentLength())
	//fmt.Println("ContentLength: ", hc.parser.ContentLength())
	if bodyLen == -1 {
		bodyLen = 0
	}

	buffer = buffer[headerOffset+bodyLen:]
	buffer = bytes.TrimSpace(buffer)

	//remainbufstr := strings.TrimSpace(uring_net.BytesToString(data.Buffer))
	//strings.ReplaceAll(remainbufstr, " ", "")
	if len(buffer) > 0 && len(buffer) != 87 {
		//if data.Buffer[0] != 0 {
		//fmt.Println("the buffer: ", remainbufstr, " the length: ", len(remainbufstr))
		log.Println(UringNet.BytesToString(buffer))
		log.Println(len(buffer))
		goto pipeline
		//}
	}
	//log.Println("data:", "head offset: ", uring_net.BytesToString(data.WriteBuf))

	//data.Bytebuffer = *bytes.NewBuffer(hc.buf)
	//copy(hc.buf, data.Buffer)

	//data.Buffer = hc.buf
	//log.Println("response length:", len(data.Buffer))
	//log.Println("response length:", len(data.WriteBuf))
	//ringnet.Write2(data.Fd, data.WriteBuf)
	//fmt.Println("buffer: ", hc.buf)
	//sqe2 := .ring.GetSQEntry()
	// claim buffer for I/O write
	//bw := ringnet.BufferPool.Get().(*[]byte)
	//bw := make([]byte, 1024)
	//sqe2.SetFlags(uring.IOSQE_IO_LINK)
	//ringnet.write(data, sqe2)
	//c.Write(hc.buf)
	//hc.buf = hc.buf[:0]

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
	ts.SetContext(&httpCodec{parser: wildcat.NewHTTPParser()})
	return nil, UringNet.None
}

func main() {
	addr := os.Args[1]

	//accptRingNet, _ := uring_net.New(uring_net.NetAddress{uring_net.TcpAddr, addr}, 500, true)

	//server := &testServer{}
	ringNets, _ := UringNet.NewMany(UringNet.NetAddress{UringNet.TcpAddr, addr}, 3200, true, 2, &testServer{}) //runtime.NumCPU()

	loop := UringNet.SetLoops(ringNets)

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
