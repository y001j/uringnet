//go:build linux
// +build linux

package uringnet

import (
	"crypto/tls"
	"fmt"
	"github.com/y001j/uringnet/errors"
	socket "github.com/y001j/uringnet/sockets"
	"github.com/y001j/uringnet/uring"
	"golang.org/x/sys/unix"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type URingNet struct {
	Addr              string                // TCP address to listen on, ":http" if empty
	Type              socket.NetAddressType //the connection type
	SocketFd          int                   //listener socket fd
	Handler           EventHandler          // It is used to handle the network event.
	TLSConfig         *tls.Config           // optional TLS config, to support TLS is under development
	ReadTimeout       time.Duration         // maximum duration before timing out read of the request, it will be used to set the socket option
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

	Autobuffer [][bufLength]byte // it is just prepared for auto buffer of io_uring

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

//var UserDataPool = sync.Pool{
//	New: func() interface{} {
//		return &UserData{}
//	},
//}

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

var increase atomic.Uint64

func makeUserData(state UserdataState) *UserData {
	userData := new(UserData)
	userData.state = uint32(state)
	userData.id = increase.Add(1)

	return userData
}

// SetUring creates an IO_Uring instance
func (ringNet *URingNet) SetUring(size uint, params *uring.IOUringParams) (ring *uring.Ring, err error) {
	thering, err := uring.Setup(size, params)
	ringNet.ring = *thering
	return thering, err
}

var paraFlags uint32

// Run2 is the core running cycle of io_uring, this function don't use auto buffer.
// TODO: Still don't have the best formula to get buffer size and SQE size.
func (ringNet *URingNet) Run2(ringing uint16) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ringNet.Handler.OnBoot(ringNet)
	//var connect_num uint32 = 0
	for {
		cqe, err := ringNet.ring.GetCQEntry(1)

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

		data, suc := ringNet.userDataList.Load(cqe.UserData())

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
			ringNet.userDataList.Delete(thedata.id)
			continue
		case uint32(accepted):
			ringNet.Handler.OnOpen(thedata)
			ringNet.EchoLoop()
			Fd := cqe.Result()
			//connect_num++
			//log.Printf("URing Number: %d Client Conn %d: \n", ringindex, connect_num)
			//log.Println("URing Number: ", ringindex, " Client Conn %d:", connect_num)

			sqe := ringNet.ring.GetSQEntry()
			//claim buffer for read
			//buffer := make([]byte, 1024) //ringnet.BufferPool.Get().(*[]byte)
			//temp := ringnet.BufferPool.Get()
			//bb := temp.(*[]byte)
			//ringNet.read2(Fd, sqe)
			ringNet.recv(Fd, sqe, ringing)

			//ringnet.read(Fd, sqe, ringindex)
			ringNet.userDataList.Delete(thedata.id)
			continue
			//recycle the buffer
			//ringnet.BufferPool.Put(thedata.buffer)
			//delete(ringnet.userDataMap, thedata.id)

		case uint32(prepareReader):
			if cqe.Result() <= 0 {
				continue
			}
			//fmt.Println(BytesToString(thedata.Buffer))
			//log.Println("the buffer:", BytesToString(thedata.Buffer))
			response(ringNet, thedata, ringing, 0)
			continue
		case uint32(PrepareWriter):
			if cqe.Result() <= 0 {
				continue
			}
			ringNet.Handler.OnWritten(*thedata)
			ringNet.userDataList.Delete(thedata.id)
			continue
		case uint32(closed):
			ringNet.Handler.OnClose(*thedata)
			//delete(ringnet.userDataMap, thedata.id)
			ringNet.userDataList.Delete(thedata.id)

		}
	}
}

// Run is the core running cycle of io_uring, this function will use auto buffer.
func (ringNet *URingNet) Run(ringing uint16) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ringNet.Handler.OnBoot(ringNet)
	//var connect_num uint32 = 0
	for {
		cqe, err := ringNet.ring.GetCQEntry(1)

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

		data, suc := ringNet.userDataList.Load(cqe.UserData())

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
			ringNet.userDataList.Delete(thedata.id)
			continue
		case uint32(accepted):
			ringNet.Handler.OnOpen(thedata)
			ringNet.EchoLoop()
			Fd := cqe.Result()
			//connect_num++
			//log.Printf("URing Number: %d Client Conn %d: \n", ringindex, connect_num)
			//log.Println("URing Number: ", ringindex, " Client Conn %d:", connect_num)

			sqe := ringNet.ring.GetSQEntry()
			//claim buffer for read
			//buffer := make([]byte, 1024) //ringnet.BufferPool.Get().(*[]byte)
			//temp := ringnet.BufferPool.Get()
			//bb := temp.(*[]byte)
			ringNet.read2(Fd, sqe)

			//ringnet.read(Fd, sqe, ringindex)
			ringNet.userDataList.Delete(thedata.id)
			continue
			//recycle the buffer
			//ringnet.BufferPool.Put(thedata.buffer)
			//delete(ringnet.userDataMap, thedata.id)

		case uint32(prepareReader):
			if cqe.Result() <= 0 {
				continue
			}
			//log.Println("the buffer:", BytesToString(thedata.Buffer))
			offset := uint64(cqe.Flags() >> uring.IORING_CQE_BUFFER_SHIFT)
			thedata.Buffer = ringNet.Autobuffer[offset][:]
			thedata.BufSize = cqe.Result()
			//fmt.Println(BytesToString(thedata.Buffer))
			//log.Println("the buffer:", BytesToString(thedata.Buffer))
			responseWithBuffer(ringNet, thedata, ringing, offset)
			continue
		case uint32(PrepareWriter):
			if cqe.Result() <= 0 {
				continue
			}
			ringNet.Handler.OnWritten(*thedata)
			ringNet.userDataList.Delete(thedata.id)
			continue
		case uint32(closed):
			ringNet.Handler.OnClose(*thedata)
			//delete(ringnet.userDataMap, thedata.id)
			ringNet.userDataList.Delete(thedata.id)
		}
	}
}

func (ringNet *URingNet) ShutDown() {
	ringNet.ring.Flush()
	ringNet.ring.Close()
	ringNet.inShutdown = 1
	ringNet.ReadBuffer = nil
	ringNet.WriteBuffer = nil
	ringNet.userDataMap = nil
	ringNet.Handler.OnShutdown(ringNet)
}

func response(ringnet *URingNet, data *UserData, gid uint16, offset uint64) {

	action := ringnet.Handler.OnTraffic(data, ringnet)

	switch action {
	case Echo: // Echo: First write and then add another read event into SQEs.

		//sqe2 := ringnet.ring.GetSQEntry()
		sqe1 := ringnet.ring.GetSQEntry()

		//ringnet.write(data, sqe2)
		//ringnet.write(data, sqe1)
		ringnet.send(data, sqe1, gid)

		sqe := ringnet.ring.GetSQEntry()

		ringnet.recv(data.Fd, sqe, gid)
		//fmt.Println("read is set for uring ", gid)

	case Read:
		sqe := ringnet.ring.GetSQEntry()
		//ringnet.read2(data.Fd, sqe)
		ringnet.recv(data.Fd, sqe, gid)
	case Write:
		sqe1 := ringnet.ring.GetSQEntry()
		//ringnet.write(data, sqe1)
		ringnet.send(data, sqe1, gid)
		_, err := ringnet.ring.Submit(0, &paraFlags)
		if err != nil {
			fmt.Println("Error Message: ", err)
		}
		//EchoAndClose type just send a write event into SQEs and then close the socket connection. the write and close event should be linked together.
	case EchoAndClose:
		sqe2 := ringnet.ring.GetSQEntry()
		// claim buffer for I/O write
		//bw := ringnet.BufferPool.Get().(*[]byte)
		//bw := make([]byte, 1024)
		//sqe2.SetFlags(uring.IOSQE_IO_LINK)
		//ringnet.write(data, sqe2)
		ringnet.send(data, sqe2, gid)
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

// Run is the core running cycle of io_uring, this function will use auto buffer.
func responseWithBuffer(ringnet *URingNet, data *UserData, gid uint16, offset uint64) {

	action := ringnet.Handler.OnTraffic(data, ringnet)

	switch action {
	case Echo: // Echo: First write and then add another read event into SQEs.

		sqe1 := ringnet.ring.GetSQEntry()
		ringnet.write(data, sqe1)

		sqe := ringnet.ring.GetSQEntry()
		ringnet.read(data.Fd, sqe, gid)
		//fmt.Println("read is set for uring ", gid)

	case Read:
		sqe := ringnet.ring.GetSQEntry()
		ringnet.read(data.Fd, sqe, gid)
	case Write:
		sqe1 := ringnet.ring.GetSQEntry()
		ringnet.write(data, sqe1)
		_, err := ringnet.ring.Submit(0, &paraFlags)
		if err != nil {
			fmt.Println("Error Message: ", err)
		}
		//EchoAndClose type just send a write event into SQEs and then close the socket connection. the write and close event should be linked together.
	case EchoAndClose:
		sqe2 := ringnet.ring.GetSQEntry()
		ringnet.write(data, sqe2)
		sqe := ringnet.ring.GetSQEntry()
		sqe.SetFlags(uring.IOSQE_IO_DRAIN)
		ringnet.close(data, sqe)
		_, err := ringnet.ring.Submit(0, &paraFlags)
		if err != nil {
			fmt.Println("Error Message: ", err)
		}
	case Close:
		sqe := ringnet.ring.GetSQEntry()
		ringnet.addBuffer(offset, gid)
		ringnet.close(data, sqe)

	}
	//  recover kernel buffer; the buffer should be restored after using.
	ringnet.addBuffer(offset, gid)
	//  remove the userdata in this loop
	ringnet.userDataList.Delete(data.id)
	//delete(ringnet.userDataMap, data.id)
}

func (ringNet *URingNet) close(thedata *UserData, sqe *uring.SQEntry) {
	data := makeUserData(closed)
	data.Fd = thedata.Fd
	ringNet.userDataList.Store(data.id, data)
	//ringnet.userDataMap[data.id] = data

	sqe.SetUserData(data.id)
	sqe.SetLen(1)
	uring.Close(sqe, uintptr(thedata.Fd))
	//return data
}

func (ringNet *URingNet) write(thedata *UserData, sqe2 *uring.SQEntry) {
	data1 := makeUserData(PrepareWriter)
	data1.Fd = thedata.Fd
	//thebuffer := make([]byte, 1024)
	//thedata.buffer = thebuffer
	//copy(thebuffer, thedata.buffer)
	ringNet.userDataList.Store(data1.id, data1)
	//ringnet.userDataMap[data1.id] = data1
	//ringnet.mu.Unlock()
	sqe2.SetUserData(data1.id)
	//sqe2.SetFlags(uring.IOSQE_IO_LINK)
	uring.Write(sqe2, uintptr(data1.Fd), thedata.WriteBuf)

	//uring.write(sqe2, uintptr(data1.Fd), thedata.Buffer) //data.WriteBuf)
	//ringnet.ring.Submit(0, &paraFlags)
}
func (ringNet *URingNet) write2(Fd int32, buffer []byte) {
	sqe2 := ringNet.ring.GetSQEntry()
	data1 := makeUserData(PrepareWriter)
	data1.Fd = Fd

	//ringnet.userDataMap[data1.id] = data1
	ringNet.userDataList.Store(data1.id, data1)
	//ringnet.mu.Unlock()
	sqe2.SetUserData(data1.id)

	uring.Write(sqe2, uintptr(data1.Fd), buffer)
	ringNet.ring.Submit(0, &paraFlags)

}

// read method when using auto buffer
func (ringNet *URingNet) read(Fd int32, sqe *uring.SQEntry, ringIndex uint16) {
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
	//uring.Read(sqe, uintptr(data2.Fd), ringnet.ReadBuffer)
	uring.ReadNoBuf(sqe, uintptr(Fd), uint32(bufLength))

	//ringnet.userDataList.Store(data2.id, data2)
	//co := conn{}
	//co.fd = data2.Fd
	//co.rawSockAddr = sqe.
	//ringnet.ringloop.connections.Store(data2.Fd)
	//ringnet.userDataMap[data2.id] = data2
	ringNet.userDataList.Store(data2.id, data2)

	//paraFlags = uring.IORING_SETUP_SQPOLL
	ringNet.ring.Submit(0, &paraFlags)
}

func (ringNet *URingNet) recv(Fd int32, sqe *uring.SQEntry, ringIndex uint16) {
	data2 := makeUserData(prepareReader)
	data2.Fd = Fd
	sqe.SetUserData(data2.id)
	uring.Recv(sqe, uintptr(Fd), ringNet.ReadBuffer, 0)
	ringNet.userDataList.Store(data2.id, data2)
	//paraFlags = uring.IORING_ENTER_SQ_WAKEUP
	//}
	ringNet.ring.Submit(0, &paraFlags)
}

func (ringNet *URingNet) send(thedata *UserData, sqe *uring.SQEntry, ringIndex uint16) {
	data2 := makeUserData(PrepareWriter)
	data2.Fd = thedata.Fd
	sqe.SetUserData(data2.id)
	uring.Send(sqe, uintptr(data2.Fd), thedata.WriteBuf, unix.MSG_DONTWAIT|unix.MSG_ZEROCOPY)
	ringNet.userDataList.Store(data2.id, data2)
}

func (ringNet *URingNet) read_multi(Fd int32, sqes []*uring.SQEntry, ringIndex uint16) {
	data2 := makeUserData(prepareReader)
	data2.Fd = Fd
	for _, sqe := range sqes {
		sqe.SetUserData(data2.id)

		//Add read event
		sqe.SetFlags(uring.IOSQE_BUFFER_SELECT)
		sqe.SetBufGroup(ringIndex)
		uring.ReadNoBuf(sqe, uintptr(Fd), uint32(bufLength))
		ringNet.userDataList.Store(data2.id, data2)
	}
	//sqes的长度如何获取:
	ringNet.ring.Submit(uint32(len(sqes)), &paraFlags)
}

// this function is used to read data from the network socket without auto buffer.
func (ringNet *URingNet) read2(Fd int32, sqe *uring.SQEntry) {
	data2 := makeUserData(prepareReader)
	data2.Fd = Fd
	sqe.SetUserData(data2.id)

	//data2.Buffer = make([]byte, 1024)
	//ringnet.userDataMap[data2.id] = data2
	ringNet.userDataList.Store(data2.id, data2)

	uring.Read(sqe, uintptr(Fd), ringNet.ReadBuffer)

	ringNet.ring.Submit(0, &paraFlags)
}

// New Creates a new uRingnNet which is used to
func New(addr NetAddress, size uint, sqpoll bool, options socket.SocketOptions) (*URingNet, error) {
	ringNet := &URingNet{}
	ringNet.userDataMap = make(map[uint64]*UserData)
	ops := socket.SetOptions(string(addr.AddrType), options)

	var err error
	switch addr.AddrType {
	case socket.Tcp, socket.Tcp4, socket.Tcp6:
		ringNet.SocketFd, _, err = socket.TCPSocket(string(addr.AddrType), addr.Address, true, ops...)
	case socket.Udp, socket.Udp4, socket.Udp6:
		ringNet.SocketFd, _, err = socket.UDPSocket(string(addr.AddrType), addr.Address, true, ops...)
	case socket.Unix:
		ringNet.SocketFd, _, err = socket.UnixSocket(string(addr.AddrType), addr.Address, true, ops...)
	default:
		return nil, errors.ErrUnsupportedProtocol
	}

	if err != nil {
		return nil, fmt.Errorf("%w: %v", errors.ErrSocketCreation, err)
	}

	if ringNet.SocketFd < 0 {
		return nil, errors.ErrInvalidSocketFd
	}

	ringNet.Addr = addr.Address
	ringNet.Type = addr.AddrType

	// Create the io_uring instance
	var params *uring.IOUringParams
	if sqpoll {
		params = &uring.IOUringParams{Flags: uring.IORING_SETUP_SQPOLL | uring.IORING_SETUP_SQ_AFF, SQThreadCPU: 1}
	}

	_, err = ringNet.SetUring(size, params)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errors.ErrRingSetup, err)
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
	var sockfd int
	ops := socket.SetOptions(string(addr.AddrType), options)

	var err error
	switch addr.AddrType {
	case socket.Tcp, socket.Tcp4, socket.Tcp6:
		sockfd, _, err = socket.TCPSocket(string(addr.AddrType), addr.Address, true, ops...)
	case socket.Udp, socket.Udp4, socket.Udp6:
		sockfd, _, err = socket.UDPSocket(string(addr.AddrType), addr.Address, true, ops...)
	case socket.Unix:
		sockfd, _, err = socket.UnixSocket(string(addr.AddrType), addr.Address, true, ops...)
	default:
		return nil, errors.ErrUnsupportedProtocol
	}

	if err != nil {
		return nil, fmt.Errorf("%w: %v", errors.ErrSocketCreation, err)
	}

	if sockfd < 0 {
		return nil, errors.ErrInvalidSocketFd
	}

	uringArray := make([]*URingNet, num)

	// Create the io_uring instances
	for i := 0; i < num; i++ {
		uringArray[i] = &URingNet{}
		uringArray[i].ReadBuffer = make([]byte, 1024)
		uringArray[i].WriteBuffer = make([]byte, 1024)
		uringArray[i].SocketFd = sockfd
		uringArray[i].Addr = addr.Address
		uringArray[i].Type = addr.AddrType
		uringArray[i].Handler = handler

		var params *uring.IOUringParams
		if sqpoll {
			params = &uring.IOUringParams{
				Flags:    uring.IORING_SETUP_SQPOLL,
				Features: uring.IORING_FEAT_NODROP | uring.IORING_FEAT_FAST_POLL | uring.IORING_FEAT_SQPOLL_NONFIXED,
			}
		} else {
			params = &uring.IOUringParams{
				Features: uring.IORING_FEAT_FAST_POLL | uring.IORING_FEAT_NODROP,
			}
		}

		_, err = uringArray[i].SetUring(size, params)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to setup uring instance %d: %v", errors.ErrRingSetup, i, err)
		}

		fmt.Printf("Uring instance %d initiated!\n", i)
	}
	return uringArray, nil
}

type NetAddress struct {
	AddrType socket.NetAddressType
	Address  string
}

// addBuffer  kernel buffer should be restored after using

func (ringNet *URingNet) addBuffer(offset uint64, gid uint16) {
	sqe := ringNet.ring.GetSQEntry()
	uring.ProvideSingleBuf(sqe, &ringNet.Autobuffer[offset], 1, uint32(bufLength), gid, offset)
	data := makeUserData(provideBuffer)
	sqe.SetUserData(data.id)
	ringNet.userDataList.Store(data.id, data)
	//ringNet.ringloop.ringNet.userDataMap[data.id] = data
	//_, _ = ringNet.ring.Submit(0, nil)
}
