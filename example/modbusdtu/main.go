//go:build linux
// +build linux

// Command modbusdtu is a DTU server: 4G/Ethernet DTUs in transparent mode connect to it, register
// with their IMEI and send heartbeats, and it polls the RS-485 device behind each one (Modbus RTU
// over the DTU's connection), printing the registers it reads.
//
//	modbusdtu -addr :9000 -register 15 -heartbeat HB -unit 1 -reg 0 -count 10 -interval 5s
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/y001j/uringnet"
	"github.com/y001j/uringnet/modbus"
)

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	regLen := flag.Int("register", 15, "registration packet length (0: none, the ID is the remote address)")
	heartbeat := flag.String("heartbeat", "", "heartbeat packet sent by the DTUs")
	tcp := flag.Bool("tcp", false, "Modbus TCP framing instead of RTU over TCP")
	unit := flag.Int("unit", 1, "unit ID of the device behind each DTU")
	reg := flag.Int("reg", 0, "first holding register to read")
	count := flag.Int("count", 10, "number of registers to read")
	interval := flag.Duration("interval", 5*time.Second, "poll interval")
	flag.Parse()

	cfg := modbus.DTUConfig{
		LinkConfig: modbus.LinkConfig{Framing: modbus.RTUOverTCP, Timeout: 2 * time.Second, Retries: 1},
		Heartbeat:  []byte(*heartbeat),
		OnOnline: func(l *modbus.Link) {
			log.Printf("DTU %s online", l.ID())
			go poll(l, byte(*unit), uint16(*reg), uint16(*count), *interval)
		},
		OnOffline: func(l *modbus.Link, err error) { log.Printf("DTU %s offline: %v", l.ID(), err) },
	}
	if *tcp {
		cfg.Framing = modbus.TCP
	}
	if *regLen > 0 {
		cfg.Register = modbus.RegisterFixed(*regLen)
	}
	s := modbus.NewDTUServer(cfg)
	srv, err := uringnet.Listen("tcp", *addr, s.Handler(), uringnet.WithTCPKeepAlive(time.Minute))
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		<-sig
		srv.Close()
	}()
	log.Printf("DTU server on %s (%v)", srv.Addr(), cfg.Framing)
	srv.Serve()
}

// poll reads the registers of the device behind a DTU until the DTU goes offline.
func poll(l *modbus.Link, unit byte, reg, count uint16, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		regs, err := l.ReadHoldingRegisters(ctx, unit, reg, count)
		cancel()
		switch {
		case errors.Is(err, modbus.ErrClosed), errors.Is(err, modbus.ErrConnLost):
			return
		case err != nil:
			log.Printf("DTU %s: %v", l.ID(), err)
		default:
			log.Printf("DTU %s: registers %d..%d = %v", l.ID(), reg, int(reg)+len(regs)-1, regs)
		}
		<-t.C
	}
}
