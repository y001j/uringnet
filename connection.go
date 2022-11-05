package UringNet

import (
	"bytes"
	"golang.org/x/sys/unix"
	"net"
)

type conn struct {
	fd             int           // file descriptor
	ctx            interface{}   // user-defined context
	peer           unix.Sockaddr // remote socket address
	loop           *Ringloop     // connected event-loop
	buffer         []byte        // buffer for the latest bytes
	opened         bool          // connection opened event fired
	localAddr      net.Addr      // local addr
	remoteAddr     net.Addr      // remote addr
	isDatagram     bool          // UDP protocol
	inboundBuffer  bytes.Buffer  //elastic.RingBuffer      // buffer for leftover data from the peer
	outboundBuffer *bytes.Buffer //*elastic.Buffer         // buffer for data that is eligible to be sent to the peer
	//pollAttachment *netpoll.PollAttachment // connection attachment for poller
	rawSockAddr unix.RawSockaddrAny
}
