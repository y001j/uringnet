package uring_net

import (
	"fmt"
	"log"
	"net"
	"uring"
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
		//data2 := makeUserData(prepareReader)
		//data2.Fd = int32(f.Fd())
		//sqe := loop.RingNet[count].ring.GetSQEntry()
		loop.RingNet[count].Fd.Store(f.Fd())

		data := makeUserData(accepted)
		loop.RingNet[count].Handler.OnOpen(data)
		//loop.RingNet[count].SocketFd = int(f.Fd())

		//loop.RingNet[count].userDataList.Store(data2.id, data2)

		//uring.Read(sqe, f.Fd(), loop.RingNet[count].ReadBuffer)
		//loop.RingNet[count].ring.Submit(0, &paraFlags)

		if count < loop.RingCount-1 {
			count++
		} else {
			count = 0
		}
	}
}

func NewManyForAcceptor(addr NetAddress, size uint, sqpoll bool, num int, handler EventHandler) ([]*URingNet, error) {
	//1. set the socket
	uringArray := make([]*URingNet, num) //*URingNet{}
	//ringNet.userDataList = make(sync.Map, 1024)
	//Create the io_uring instance
	for i := 0; i < num; i++ {
		uringArray[i] = &URingNet{}
		//uringArray[i].userDataMap = make(map[uint64]*UserData)
		uringArray[i].ReadBuffer = make([]byte, 2048)
		uringArray[i].WriteBuffer = make([]byte, 1024)
		//uringArray[i].SocketFd = sockfd
		uringArray[i].Addr = addr.Address
		uringArray[i].Type = addr.AddrType
		uringArray[i].Handler = handler

		if sqpoll {
			uringArray[i].SetUring(size, &uring.IOUringParams{Flags: uring.IORING_SETUP_SQPOLL, Features: uring.IORING_FEAT_FAST_POLL}) //Features: uring.IORING_FEAT_FAST_POLL})
		} else {
			uringArray[i].SetUring(size, nil)
		}
		fmt.Println("Uring instance initiated!")
	}
	return uringArray, nil
}
