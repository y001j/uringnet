// Package uring_net
// @Description:
package UringNet

import (
	socket "UringNet/sockets"
	"crypto/tls"
	"fmt"
	"golang.org/x/sys/unix"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
	"uring"
)

type URingNet struct {
	Addr     string                // TCP address to listen on, ":http" if empty
	Type     socket.NetAddressType //the connection type
	SocketFd int                   //listener socket fd
	//Handler           Handler // handler to invoke, http.DefaultServeMux if nil
	Handler           EventHandler
	TLSConfig         *tls.Config
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
	Fd                atomic.Uintptr
	//TLSNextProto      map[string]func(*URingNet, *tls.Conn, Handler)
	//ConnState         func(net.Conn, ConnState)
	ErrorLog *log.Logger

	disableKeepAlives int32 // accessed atomically.
	inShutdown        int32
	Count             uint32
	nextProtoOnce     sync.Once
	nextProtoErr      error
	ring              uring.Ring
	userDataList      sync.Map // all the userdata
	userDataMap       map[uint64]*UserData
	ReadBuffer        []byte
	WriteBuffer       []byte

	Autobuffer [][bufLength]byte

	ringloop *Ringloop

	mu sync.Mutex
	//listeners map[*net.Listener]struct{}
	//activeConn map[*conn]struct{} // 活跃连接
	//doneChan   chan struct{}
	onShutdown []func()
	//BufferPool sync.Pool
}

type NetAddressType int

type UserdataState uint32

const (
	accepted      UserdataState = iota // 0. the socket is accepted, that means the network socket is established
	prepareReader                      // 1. network read is completed
	PrepareWriter                      // 2. network write is completed
	closed                             // 3. the socket is closed.
	provideBuffer                      // 4. buffer has been created.
)

type UserData struct {
	id uint64

	//resulter chan<- Result
	opcode uint8

	//ReadBuf  []byte
	WriteBuf []byte //bytes.Buffer

	state uint32 //userdataState
	//ringNet *URingNet
	Fd        int32
	Buffer    []byte
	BufOffset uint64
	BufSize   int32

	// for accept socket
	ClientSock *syscall.RawSockaddrAny
	socklen    *uint32
	//Bytebuffer bytes.Buffer

	//r0 interface{}
	//R1 interface{}

	//client unix.RawSockaddrAny
	//holds   []interface{}
	//request *request
}

//var UserDataList sync.Map

// var Buffers [1024][1024]byte

// SetState change the state of unique userdata
func (data *UserData) SetState(state UserdataState) {
	atomic.StoreUint32(&data.state, uint32(state))
}

type request struct {
	ringNet URingNet
	done    chan struct{}
}

var increase uint64 = 1

func makeUserData(state UserdataState) *UserData {
	defer func() {
		err := recover() // 内置函数，可以捕获异常
		if err != nil {
			fmt.Println("err:", err)
			fmt.Println("发生异常............")
		}
	}()

	userData := new(UserData)
	//userData := &UserData{
	//	//ringNet: ringNet,
	//	state: uint32(state),
	//}
	userData.state = uint32(state)

	//userData.id = uint64(uintptr(unsafe.Pointer(userData)))
	//userData.Bytebuffer = new(bytes.Buffer)
	//userData.id = increase
	//rand.Seed(int64(uintptr(unsafe.Pointer(userData))))
	//time.Now().UnixNano()
	userData.id = increase
	//log.Println("userdata id", increase)
	increase++
	//userData.request.id = userData.id

	return userData
}

func makeUserData2(state UserdataState) UserData {
	userData := UserData{
		//ringNet: ringNet,
		state: uint32(state),
	}

	userData.id = uint64(uintptr(unsafe.Pointer(&userData)))
	//userData.request.id = userData.id
	return userData
}

// SetUring creates an IO_Uring instance
func (ringnet *URingNet) SetUring(size uint, params *uring.IOUringParams) (ring *uring.Ring, err error) {
	thering, err := uring.Setup(size, params)
	ringnet.ring = *thering
	return thering, err
}

var paraFlags uint32

// It will Run with Kernel buffer, we should set a proper buffer size.
// TODO: Still don't have the best formula to get buffer size and SQE size.
func (ringnet *URingNet) Run2(ringindex uint16) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ringnet.Handler.OnBoot(*ringnet)
	var connect_num uint32 = 0
	for {
		cqe, err := ringnet.ring.GetCQEntry(1)

		//defer ringnet.ring.Close()
		// have accepted
		//theFd := ringnet.Fd.Load()
		//if theFd != 0 {
		//	sqe := ringnet.ring.GetSQEntry()
		//	ringnet.read(int32(theFd), sqe, ringindex)
		//	ringnet.Fd.Store(0)
		//}

		if err != nil {
			if err == unix.EAGAIN {
				//log.Println("Completion queue is empty!")
				continue
			}
			//log.Println("uring has fatal error! ", err)
			continue
		}

		data, suc := ringnet.userDataList.Load(cqe.UserData())

		//data, suc := ringnet.userDataMap[cqe.UserData()]
		if !suc {
			//log.Println("Cannot find matched userdata!")
			//ringnet.ring.Flush()
			continue
		}

		thedata := (data).(*UserData)

		//ioc := unix.Iovec{}
		//ioc.SetLen(1)
		switch thedata.state {
		case uint32(provideBuffer):
			ringnet.userDataList.Delete(thedata.id)
			continue
		case uint32(accepted):
			ringnet.Handler.OnOpen(thedata)
			ringnet.EchoLoop()
			Fd := cqe.Result()
			connect_num++
			//log.Printf("URing Number: %d Client Conn %d: \n", ringindex, connect_num)
			log.Println("URing Number: ", ringindex, " Client Conn %d:", connect_num)

			sqe := ringnet.ring.GetSQEntry()
			//claim buffer for read
			//buffer := make([]byte, 1024) //ringnet.BufferPool.Get().(*[]byte)
			//temp := ringnet.BufferPool.Get()
			//bb := temp.(*[]byte)
			//ringnet.read2(Fd, sqe)

			ringnet.read(Fd, sqe, ringindex)
			continue
			//recycle the buffer
			//ringnet.BufferPool.Put(thedata.buffer)
			//delete(ringnet.userDataMap, thedata.id)
			ringnet.userDataList.Delete(thedata.id)
		case uint32(prepareReader):
			if cqe.Result() <= 0 {
				continue
			}
			//log.Println("the buffer:", BytesToString(thedata.Buffer))
			offset := uint64(cqe.Flags() >> uring.IORING_CQE_BUFFER_SHIFT)
			thedata.Buffer = ringnet.Autobuffer[offset][:]
			thedata.BufSize = cqe.Result()
			//fmt.Println(BytesToString(thedata.Buffer))
			//log.Println("the buffer:", BytesToString(thedata.Buffer))
			response(ringnet, thedata, ringindex, offset)
			continue
		case uint32(PrepareWriter):
			if cqe.Result() <= 0 {
				continue
			}
			ringnet.Handler.OnWritten(*thedata)
			ringnet.userDataList.Delete(thedata.id)
			continue
		case uint32(closed):
			ringnet.Handler.OnClose(*thedata)
			//delete(ringnet.userDataMap, thedata.id)
			ringnet.userDataList.Delete(thedata.id)
		}
	}
}

func (ringnet *URingNet) ShutDown() {
	ringnet.ring.Flush()
	ringnet.ring.Close()
	ringnet.inShutdown = 1
	ringnet.ReadBuffer = nil
	ringnet.WriteBuffer = nil
	ringnet.userDataMap = nil
	ringnet.Handler.OnShutdown(*ringnet)
}

func response(ringnet *URingNet, data *UserData, gid uint16, offset uint64) {

	action := ringnet.Handler.OnTraffic(data, *ringnet)

	//var data2 *UserData

	switch action {
	case Echo: // Echo: First write and then add another read event into SQEs.
		//fmt.Println("the buffer index ", offset)
		//ringnet.addBuffer(offset)
		//temp := BytesToString2(Buffers[cqe.Flags()>>uring.IORING_CQE_BUFFER_SHIFT])
		//temp := Buffers[cqe.Flags()>>uring.IORING_CQE_BUFFER_SHIFT][:]
		//fmt.Println(BytesToString(temp))
		/// Add read event

		//prepare write
		//ringnet.Count++
		//println("the worker run times: ", ringnet.Count)

		sqe2 := ringnet.ring.GetSQEntry()
		// claim buffer for I/O write
		//bw := ringnet.BufferPool.Get().(*[]byte)
		//bw := make([]byte, 1024)
		//sqe2.SetFlags(uring.IOSQE_IO_LINK)
		ringnet.Write(data, sqe2)
		// "head offset: ", headerOffset)
		sqe := ringnet.ring.GetSQEntry()
		// claim buffer for I/O read
		//temp := ringnet.BufferPool.Get()
		//br := temp.(*[]byte)
		//br := make([]byte, 1024)
		//br := bufferpool.Get().(*[]byte)
		ringnet.addBuffer(offset, gid)
		ringnet.read(data.Fd, sqe, gid)
		//sqe.SetFlags(uring.IOSQE_ASYNC)
		//ringnet.read2(data.Fd, sqe)

		// recycle buffer
		//ringnet.BufferPool.Put(thedata.buffer)
		//ringnet.BufferPool.Put(thedata.buffer)

	//ringnet.ringloop.GetBuffer()[offset]
	//writeBuf = ringnet.ringloop.GetBuffer()[offset][:] //[]byte("HTTP/1.1 200 OK\r\nContent-Length: 50\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n<title>OK</title><body>I am great to do it!</body>") //Date: Sat, 17 Sep 2022 07:54:22 GMT
	//buf := ringnet.ringloop.buffer[offset][:]
	//StringToBytes2
	//ringnet.ringloop.buffer[offset]

	//test := append(buf, data.WriteBuf)
	//sqe.SetAddr()
	//EchoAndClose type just send a write event into SQEs and then close the socket connection. the write and close event should be linked together.
	case Read: // Echo: First write and then add another read event into SQEs.
		sqe := ringnet.ring.GetSQEntry()
		//log.Println(sqe.UserData())
		ringnet.read(data.Fd, sqe, gid)
	case EchoAndClose:
		sqe2 := ringnet.ring.GetSQEntry()
		// claim buffer for I/O write
		//bw := ringnet.BufferPool.Get().(*[]byte)
		//bw := make([]byte, 1024)
		//sqe2.SetFlags(uring.IOSQE_IO_LINK)
		ringnet.Write(data, sqe2)
		sqe := ringnet.ring.GetSQEntry()
		sqe.SetFlags(uring.IOSQE_IO_DRAIN)
		ringnet.close(data, sqe)
		_, err := ringnet.ring.Submit(0, &paraFlags)
		if err != nil {
			fmt.Println("Error Message: ", err)
		}
	case Close:
		sqe := ringnet.ring.GetSQEntry()
		ringnet.close(data, sqe)
	}
	//  recover kernel buffer; the buffer should be restored after using.

	//  remove the userdata in this loop
	//data.Buffer = nil
	//data.WriteBuf = nil
	ringnet.userDataList.Delete(data.id)
	//delete(ringnet.userDataMap, data.id)
}

func (ringnet *URingNet) close(thedata *UserData, sqe *uring.SQEntry) {
	data := makeUserData(closed)
	data.Fd = thedata.Fd
	ringnet.userDataList.Store(data.id, data)
	//ringnet.userDataMap[data.id] = data

	sqe.SetUserData(data.id)
	sqe.SetLen(1)
	uring.Close(sqe, uintptr(thedata.Fd))
	//return data
}

func (ringnet *URingNet) Write(thedata *UserData, sqe2 *uring.SQEntry) {
	data1 := makeUserData(PrepareWriter)
	data1.Fd = thedata.Fd
	//thebuffer := make([]byte, 1024)
	//thedata.buffer = thebuffer
	//copy(thebuffer, thedata.buffer)
	ringnet.userDataList.Store(data1.id, data1)
	//ringnet.userDataMap[data1.id] = data1
	//ringnet.mu.Unlock()
	sqe2.SetUserData(data1.id)
	//sqe2.SetFlags(uring.IOSQE_IO_LINK)
	uring.Write(sqe2, uintptr(data1.Fd), thedata.WriteBuf)
	//ringnet.ring.Submit(0, &paraFlags)
	//uring.Write(sqe2, uintptr(data1.Fd), thedata.Buffer) //data.WriteBuf)
	//ringnet.ring.Submit(0, &paraFlags)
}
func (ringnet *URingNet) Write2(Fd int32, buffer []byte) {
	sqe2 := ringnet.ring.GetSQEntry()
	data1 := makeUserData(PrepareWriter)
	data1.Fd = Fd

	//ringnet.userDataMap[data1.id] = data1
	ringnet.userDataList.Store(data1.id, data1)
	//ringnet.mu.Unlock()
	sqe2.SetUserData(data1.id)

	uring.Write(sqe2, uintptr(data1.Fd), buffer)
	ringnet.ring.Submit(0, &paraFlags)

}

// read
//
//	@Description:
//	@receiver ringnet
//	@param thedata
//	@param sqe
//	@param ioc
func (ringnet *URingNet) read(Fd int32, sqe *uring.SQEntry, ringIndex uint16) {
	data2 := makeUserData(prepareReader)
	data2.Fd = Fd
	//data2.buffer = make([]byte, 1024)
	//data2.bytebuffer = buffer
	//data2.client = thedata.client
	sqe.SetUserData(data2.id)

	//ioc := unix.Iovec{}
	//ioc.SetLen(1)

	//Add read event
	sqe.SetFlags(uring.IOSQE_BUFFER_SELECT)
	sqe.SetBufGroup(ringIndex)
	//uring.Read(sqe, uintptr(thedata.Fd), ringnet.ReadBuffer)
	uring.ReadNoBuf(sqe, uintptr(Fd), uint32(bufLength))

	//ringnet.userDataList.Store(data2.id, data2)
	//co := conn{}
	//co.fd = data2.Fd
	//co.rawSockAddr = sqe.
	//ringnet.ringloop.connections.Store(data2.Fd)
	//ringnet.userDataMap[data2.id] = data2
	ringnet.userDataList.Store(data2.id, data2)

	//paraFlags = uring.IORING_SETUP_SQPOLL
	ringnet.ring.Submit(0, &paraFlags)
}

func (ringnet *URingNet) read2(Fd int32, sqe *uring.SQEntry) {
	data2 := makeUserData(prepareReader)
	data2.Fd = Fd
	sqe.SetUserData(data2.id)

	//data2.Buffer = make([]byte, 1024)
	//ringnet.userDataMap[data2.id] = data2
	_, loaded := ringnet.userDataList.LoadOrStore(data2.id, data2)
	if loaded {
		log.Println("data has been loaded!")
	}
	//sqe.SetLen(1)
	uring.Read(sqe, uintptr(Fd), ringnet.ReadBuffer)

	ringnet.ring.Submit(0, &paraFlags)
}

// New Creates a new uRingnNet which is used to
func New(addr NetAddress, size uint, sqpoll bool, options socket.SocketOptions) (*URingNet, error) {
	//1. set the socket
	//var ringNet *URingNet
	ringNet := &URingNet{}
	ringNet.userDataMap = make(map[uint64]*UserData)
	ops := socket.SetOptions(string(addr.AddrType), options)
	switch addr.AddrType {
	case socket.Tcp, socket.Tcp4, socket.Tcp6:
		ringNet.SocketFd, _, _ = socket.TCPSocket(string(addr.AddrType), addr.Address, true, ops...) //ListenTCPSocket(addr)
	case socket.Udp, socket.Udp4, socket.Udp6:
		ringNet.SocketFd, _, _ = socket.UDPSocket(string(addr.AddrType), addr.Address, true, ops...)
	case socket.Unix:
		ringNet.SocketFd, _, _ = socket.UnixSocket(string(addr.AddrType), addr.Address, true, ops...)

	default:
		ringNet.SocketFd = -1
	}
	ringNet.Addr = addr.Address
	ringNet.Type = addr.AddrType

	//ringNet.userDataList = make(sync.Map, 1024)
	//Create the io_uring instance
	if sqpoll {
		ringNet.SetUring(size, &uring.IOUringParams{Flags: uring.IORING_SETUP_SQPOLL | uring.IORING_SETUP_SQ_AFF, SQThreadCPU: 1})
	} else {
		ringNet.SetUring(size, nil)
	}
	return ringNet, nil
}

// NewMany Create multiple uring instances
//
//	@Description:
//	@param addr
//	@param size set SQ size
//	@param sqpoll if set sqpoll to true, io_uring submit SQs automatically  without enter syscall.
//	@param num number of io_uring instances need to be created
//	@return *[]URingNet
//	@return error
func NewMany(addr NetAddress, size uint, sqpoll bool, num int, options socket.SocketOptions, handler EventHandler) ([]*URingNet, error) {
	//1. set the socket
	var sockfd int
	ops := socket.SetOptions(string(addr.AddrType), options)
	switch addr.AddrType {
	case socket.Tcp, socket.Tcp4, socket.Tcp6:
		sockfd, _, _ = socket.TCPSocket(string(addr.AddrType), addr.Address, true, ops...) //ListenTCPSocket(addr)
	case socket.Udp, socket.Udp4, socket.Udp6:
		sockfd, _, _ = socket.UDPSocket(string(addr.AddrType), addr.Address, true, ops...)
	case socket.Unix:
		sockfd, _, _ = socket.UnixSocket(string(addr.AddrType), addr.Address, true, ops...)
	default:
		sockfd = -1
	}
	uringArray := make([]*URingNet, num) //*URingNet{}
	//ringNet.userDataList = make(sync.Map, 1024)
	//Create the io_uring instance
	for i := 0; i < num; i++ {
		uringArray[i] = &URingNet{}
		//uringArray[i].userDataMap = make(map[uint64]*UserData)
		uringArray[i].ReadBuffer = make([]byte, 1024)
		uringArray[i].WriteBuffer = make([]byte, 1024)
		uringArray[i].SocketFd = sockfd
		uringArray[i].Addr = addr.Address
		uringArray[i].Type = addr.AddrType
		uringArray[i].Handler = handler

		if sqpoll {
			uringArray[i].SetUring(size, &uring.IOUringParams{Flags: uring.IORING_SETUP_SQPOLL, Features: uring.IORING_FEAT_FAST_POLL | uring.IORING_FEAT_NODROP}) //Features: uring.IORING_FEAT_FAST_POLL})
		} else {
			uringArray[i].SetUring(size, &uring.IOUringParams{Features: uring.IORING_FEAT_FAST_POLL | uring.IORING_FEAT_NODROP})
		}
		//uringArray[i].BufferPool = sync.Pool{
		//	// New optionally specifies a function to generate
		//	// a value when Get would otherwise return nil.
		//	New: func() interface{} {
		//		buf := make([]byte, 1024)
		//		return &buf
		//	},
		//}
		fmt.Println("Uring instance initiated!")
	}
	return uringArray, nil
}

func DocSyncTaskCronJob() {
	ticker := time.NewTicker(time.Millisecond * 500) // every 0.5 seconds
	for range ticker.C {
		ProcTask()
	}
}

func ProcTask() {
	runtime.GC()
}

type NetAddress struct {
	AddrType socket.NetAddressType
	Address  string
}

// addBuffer  kernel buffer need to be restored after using
//
//	@Description:
//	@receiver ringNet
//	@param offset
func (ringNet *URingNet) addBuffer(offset uint64, gid uint16) {
	sqe := ringNet.ring.GetSQEntry()
	uring.ProvideSingleBuf(sqe, &ringNet.Autobuffer[offset], 1, uint32(bufLength), gid, offset)
	data := makeUserData(provideBuffer)
	sqe.SetUserData(data.id)
	ringNet.userDataList.Store(data.id, data)
	//ringNet.ringloop.ringNet.userDataMap[data.id] = data
	//_, _ = ringNet.ring.Submit(0, nil)
}

// Listen the TCP socket
func ListenTCPSocket(addr NetAddress) int {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		panic(err)
	}
	netAddr, err := net.ResolveTCPAddr("tcp", addr.Address)
	//tcpAddr, err := net.ResolveTCPAddr("tcp", addr.address)
	if err != nil {
		panic(err)
	}
	sockaddr := &unix.SockaddrInet4{Port: netAddr.Port}
	copy(sockaddr.Addr[:], netAddr.IP.To4())
	if err := unix.Bind(fd, sockaddr); err != nil {
		panic(err)
	}
	if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
		panic(err)
	}
	return fd
}

// Listen the UDP socket
func listenUDPSocket(addr NetAddress) int {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		panic(err)
	}
	netAddr, err := net.ResolveUDPAddr("udp", addr.Address)
	if err != nil {
		panic(err)
	}
	sockaddr := &unix.SockaddrInet4{Port: netAddr.Port}
	copy(sockaddr.Addr[:], netAddr.IP.To4())
	if err := unix.Bind(fd, sockaddr); err != nil {
		panic(err)
	}
	if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
		panic(err)
	}
	return fd
}
