package main

import (
	"bytes"
	uringnet "github.com/y001j/UringNet"
	socket "github.com/y001j/UringNet/sockets"
	"os"
	"sync"
	"time"
)

type testServer struct {
	uringnet.BuiltinEventEngine

	testloop *uringnet.Ringloop
	//ring      *uring_net.URingNet
	addr      string
	multicore bool
}

type httpCodec struct {
	delimiter []byte
	buf       []byte
}

func appendResponse(hc *[]byte) {
	*hc = append(*hc, "HTTP/1.1 200 OK\r\nServer: uringNet\r\nContent-Type: text/plain\r\nDate: "...)
	*hc = time.Now().AppendFormat(*hc, "Mon, 02 Jan 2006 15:04:05 GMT")
	*hc = append(*hc, "\r\nContent-Length: 12\r\n\r\nHello World!"...)
}

func (ts *testServer) OnTraffic(data *uringnet.UserData, ringnet *uringnet.URingNet) uringnet.Action {

	//将data.Buffer转换为string
	//buffer := data.Buffer[:data.BufSize]

	buffer := ringnet.ReadBuffer
	//tes :=
	//fmt.Println("data:", " offset: ", tes, " ", data.BufOffset)
	//获取tes中“\r\n\r\n”的数量
	count := bytes.Count(buffer, []byte("GET"))
	if count == 0 {

		return uringnet.None
	} else {
		for i := 0; i < count; i++ {
			appendResponse(&data.WriteBuf)
		}
	}
	return uringnet.Echo
}

func (ts *testServer) OnWritten(data uringnet.UserData) uringnet.Action {

	return uringnet.None
}

//func (hc *httpCodec) parse(data []byte) (int, error) {
//	if idx := bytes.Index(data, hc.delimiter); idx != -1 {
//		return idx + 4, nil
//	}
//	return -1, errCRLFNotFound
//}
//
//func (hc *httpCodec) reset() {
//	hc.buf = hc.buf[:0]
//}
//
//var errCRLFNotFound = errors.New("CRLF not found")

func (ts *testServer) OnOpen(data *uringnet.UserData) ([]byte, uringnet.Action) {

	ts.SetContext(&httpCodec{delimiter: []byte("\r\n\r\n")})
	return nil, uringnet.None
}

func main() {
	addr := os.Args[1]
	//runtime.GOMAXPROCS(runtime.NumCPU())

	options := socket.SocketOptions{TCPNoDelay: socket.TCPNoDelay, ReusePort: true}
	ringNets, _ := uringnet.NewMany(uringnet.NetAddress{socket.Tcp4, addr}, 3200, true, 1, options, &testServer{}) //runtime.NumCPU()

	loop := uringnet.SetLoops(ringNets, 4000)

	loop.RunMany()

	var waitgroup sync.WaitGroup
	waitgroup.Add(1)
	waitgroup.Wait()
}
