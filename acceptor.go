package UringNet

import (
	"fmt"
	socket "github.com/y001j/UringNet/sockets"
	"github.com/y001j/UringNet/uring"
	"golang.org/x/sys/unix"
	"log"
	"net"
	"syscall"
)

var netFdChan = make(chan int, 10)

// create a factory() to be used with channel based pool
func (loop *Ringloop) GetConnects(NetAddr string) {
	addr, err := net.ResolveTCPAddr("tcp", NetAddr)
	if err != nil {
		log.Println("parse addr error on ", addr.String())
		return
	}
	tcpServe, err := net.ListenTCP("tcp", addr)
	if err != nil {
		log.Println("Listen error on ", tcpServe.Addr().String())
		return
	}
	defer tcpServe.Close()

	fmt.Println("start listen for client...", tcpServe.Addr().String())
	var count int32 = 0
	for {
		conn, err := tcpServe.AcceptTCP()
		if err != nil {
			fmt.Println("tcp server accept error ", err)
			break
		}
		fmt.Println("Conn come in: ", conn.RemoteAddr().String())

		conn.SetNoDelay(true)
		conn.SetKeepAlive(true)

		f, err := conn.File()
		if err != nil {
			fmt.Println("tcp server accept error ", err)
			break
		}
		data2 := makeUserData(prepareReader)
		data2.Fd = int32(f.Fd())

		loop.RingNet[count].Fd.Store(f.Fd())

		data := makeUserData(accepted)
		loop.RingNet[count].Handler.OnOpen(data)
		//loop.RingNet[count].SocketFd = int(f.Fd())

		sqe := loop.RingNet[count].ring.GetSQEntry()
		sqe.SetUserData(data2.id)
		sqe.SetFlags(uring.IOSQE_BUFFER_SELECT | uring.IOSQE_ASYNC | uring.IOSQE_IO_DRAIN)
		sqe.SetBufGroup(uint16(count))
		//uring.Read(sqe, uintptr(thedata.Fd), ringnet.ReadBuffer)
		uring.ReadNoBuf(sqe, f.Fd(), uint32(bufLength))

		//uring.Read(sqe, f.Fd(), loop.RingNet[count].ReadBuffer)
		loop.RingNet[count].ring.Submit(0, &paraFlags)

		loop.RingNet[count].userDataList.Store(data2.id, data2)

		if count < loop.RingCount-1 {
			count++
		} else {
			count = 0
		}
	}
}

// creat a channel for connections
var cons = make(chan int, 32)
var accept_sequence = 0

func Acceptor(addr NetAddress, options socket.SocketOptions, num int) {
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

	// 监听套接字
	err := syscall.Listen(sockfd, syscall.SOMAXCONN)
	if err != nil {
		fmt.Println("Error listening:", err)
		return
	}
	fmt.Println("Listening on ", addr.Address)

	for {
		// 更具num进行循环
		if accept_sequence < num-1 {
			accept_sequence++
		} else {
			accept_sequence = 0
		}

		// 接受传入连接
		cfd, _, err := syscall.Accept(sockfd)
		if err != nil {
			if err == syscall.EAGAIN {
				continue
			} else {
				fmt.Println("Error accepting: ", err)
				return
			}
		}
		cons <- cfd
	}
}

func CreateRings(addr NetAddress, size uint, sqpoll bool, num int, handler EventHandler, sockfd int) ([]*URingNet, error) {
	uringArray := make([]*URingNet, num) //*URingNet{}
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
			uringArray[i].SetUring(size, &uring.IOUringParams{Flags: uring.IORING_SETUP_SQPOLL, Features: uring.IORING_FEAT_FAST_POLL | uring.IORING_FEAT_NODROP | uring.IORING_FEAT_SINGLE_MMAP}) //Features: uring.IORING_FEAT_FAST_POLL})
		} else {
			uringArray[i].SetUring(size, &uring.IOUringParams{Features: uring.IORING_FEAT_FAST_POLL | uring.IORING_FEAT_NODROP | uring.IORING_FEAT_SINGLE_MMAP})
		}
		fmt.Println("Uring instance initiated!")
	}
	return uringArray, nil
}

func (ringNet *URingNet) EchoAcceptor(cfd int) {

	sqe := ringNet.ring.GetSQEntry()
	ringNet.read2(int32(cfd), sqe)
	_, err := ringNet.ring.Submit(0, &paraFlags)
	//fmt.Println("echo server running...")
	if err != nil {
		fmt.Printf("prep request error: %v\n", err)
		return
	}
}

func (loop *Ringloop) RunManyAcceptor(addr NetAddress, options socket.SocketOptions) {
	go Acceptor(addr, options, int(loop.RingCount))
	for i := 0; i < int(loop.RingCount); i++ {
		go loop.RingNet[i].RunWithAcceptor(i)
	}

}

// Run2 is the core running cycle of io_uring, this function don't use auto buffer.
// TODO: Still don't have the best formula to get buffer size and SQE size.
func (ringNet *URingNet) RunWithAcceptor(sequence int) {
	ringNet.Handler.OnBoot(*ringNet)
	//var connect_num uint32 = 0
	for {
		//if sequence == accept_sequence {
		select {
		case cfd := <-cons:

			ringNet.Handler.OnOpen(nil)
			sqe := ringNet.ring.GetSQEntry()
			ringNet.read2(int32(cfd), sqe)
			//fmt.Println("echo server ", sequence, " running...", cfd)

		default:
			//fmt.Println("echo server running...", sequence)
		}
		//}

		// 1. get a CQE in the completion queue,
		cqe, err := ringNet.ring.GetCQEntry(0)

		// 2. if there is no CQE, then continue to get CQE
		if err != nil {
			// 2.1 if there is no CQE, except EAGAIN, then continue to get CQE
			if err == unix.EAGAIN {
				//log.Println("Completion queue is empty!")
				continue
			}
			//log.Println("uring has fatal error! ", err)
			continue
		}

		// 3. get the userdata from the map,
		data, suc := ringNet.userDataList.Load(cqe.UserData())
		if !suc {
			continue
		}

		thedata := (data).(*UserData)
		switch thedata.state {
		case uint32(provideBuffer):
			ringNet.userDataList.Delete(thedata.id)
			continue
		case uint32(prepareReader):
			if cqe.Result() <= 0 {
				continue
			}

			action := ringNet.Handler.OnTraffic(thedata, *ringNet)
			response(ringNet, thedata, action)
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
			ringNet.userDataList.Delete(thedata.id)
			continue
		}
	}
}
