package UringNet

import (
	"fmt"
	"github.com/y001j/UringNet/uring"
)

// create a factory() to be used with channel based pool

func NewManyForAcceptor(addr NetAddress, size uint, sqpoll bool, num int, handler EventHandler) ([]*URingNet, error) {
	//1. set the socket
	uringArray := make([]*URingNet, num) //*URingNet{}
	//ringNet.userDataList = make(sync.Map, 1024)
	//Create the io_uring instance
	for i := 0; i < num; i++ {
		uringArray[i] = &URingNet{}
		//uringArray[i].userDataMap = make(map[uint64]*UserData)
		//uringArray[i].ReadBuffer = make([]byte, 1024)
		//uringArray[i].WriteBuffer = make([]byte, 1024)
		//uringArray[i].SocketFd = sockfd
		uringArray[i].Addr = addr.Address
		uringArray[i].Type = addr.AddrType
		uringArray[i].Handler = handler

		if sqpoll {
			uringArray[i].SetUring(size, &uring.IOUringParams{Flags: uring.IORING_SETUP_SQPOLL, Features: uring.IORING_FEAT_FAST_POLL | uring.IORING_FEAT_NODROP}) //Features: uring.IORING_FEAT_FAST_POLL})
		} else {
			uringArray[i].SetUring(size, &uring.IOUringParams{Features: uring.IORING_FEAT_FAST_POLL | uring.IORING_FEAT_NODROP})
		}
		fmt.Println("Uring instance initiated!")
	}
	return uringArray, nil
}
