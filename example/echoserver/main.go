package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/y001j/uringnet"
)

type echoServer struct {
	uringnet.BuiltinHandler
}

// OnTraffic runs whenever data arrives. data is only valid during the call; it lies in the receive buffer,
// so writing it back sends it straight from there, without a copy.
func (echoServer) OnTraffic(c *uringnet.Conn, data []byte) uringnet.Action {
	c.Write(data)
	return uringnet.None
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: echoserver <address>")
		fmt.Println("Example: echoserver :8080")
		os.Exit(1)
	}

	srv, err := uringnet.Listen("tcp", os.Args[1], echoServer{},
		// 256 receive buffers of 16KB per ring: they are used in FIFO order, so more buffers mean colder caches; enough is best
		uringnet.WithBuffers(256, 16<<10),
		uringnet.WithIdleTimeout(5*time.Minute),
		uringnet.WithLogger(slog.Default()),
		// Optional: uringnet.WithBusyPoll(50*time.Microsecond) for lower latency, uringnet.WithZeroCopy(16<<10) for zero-copy sends of large data
	)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Printf("Echo server listening on %s\n", srv.Addr())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx) // stop accepting; close each connection once what was written has been sent
	}()
	if err := srv.Serve(); !errors.Is(err, uringnet.ErrServerClosed) {
		fmt.Println(err)
		os.Exit(1)
	}
}
