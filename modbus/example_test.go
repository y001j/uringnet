//go:build linux
// +build linux

package modbus_test

import (
	"context"
	"log"
	"time"

	"github.com/y001j/uringnet"
	"github.com/y001j/uringnet/modbus"
)

// Poll a PLC over Modbus TCP. The Link reconnects by itself if the PLC goes away.
func ExampleClient() {
	c, err := modbus.NewClient(modbus.ClientConfig{LinkConfig: modbus.LinkConfig{Timeout: time.Second, Retries: 2}})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	plc := c.Dial("192.168.1.10:502")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	regs, err := plc.ReadHoldingRegisters(ctx, 1, 0, 10)
	if err != nil {
		log.Fatal(err)
	}
	log.Println(regs)
	plc.WriteSingleCoil(ctx, 1, 4, true)
}

// Accept 4G DTUs that register with their IMEI and send "HB" heartbeats, and read the meter behind
// each one over Modbus RTU.
func ExampleDTUServer() {
	s := modbus.NewDTUServer(modbus.DTUConfig{
		LinkConfig: modbus.LinkConfig{Framing: modbus.RTUOverTCP, Timeout: 2 * time.Second},
		Register:   modbus.RegisterFixed(15),
		Heartbeat:  []byte("HB"),
		OnOnline: func(l *modbus.Link) {
			go func() { // callbacks must not block
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				regs, err := l.ReadInputRegisters(ctx, 1, 0, 4)
				log.Println(l.ID(), regs, err)
			}()
		},
	})
	srv, err := uringnet.Listen("tcp", ":9000", s.Handler())
	if err != nil {
		log.Fatal(err)
	}
	go srv.Serve()

	// Later: any online DTU by its ID
	if l := s.Link("860000000000001"); l != nil {
		l.WriteSingleRegister(context.Background(), 1, 10, 500)
	}
}

// A Modbus TCP slave whose registers the application updates.
func ExampleServer() {
	m := modbus.NewMemory(16, 16, 100, 100)
	m.OnWrite = func(unit byte, coils bool, addr uint16, n int) { log.Println("master wrote", addr, n) }
	srv, err := uringnet.Listen("tcp", ":502", modbus.NewServer(m, modbus.TCP).Handler())
	if err != nil {
		log.Fatal(err)
	}
	go srv.Serve()
	m.SetInputRegisters(0, []uint16{231, 229, 230}) // e.g. voltages
}
