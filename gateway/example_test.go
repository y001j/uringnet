//go:build linux
// +build linux

package gateway_test

import (
	"log"
	"time"

	"github.com/y001j/uringnet"
	"github.com/y001j/uringnet/gateway"
	"github.com/y001j/uringnet/modbus"
	"github.com/y001j/uringnet/mqtt"
)

// A gateway for meters behind 4G DTUs: every DTU that comes online is polled with the same point
// table, and the values appear on devices/<IMEI>/<point>.
func Example() {
	broker := mqtt.NewBroker(mqtt.Config{SessionExpiry: 24 * time.Hour})
	mqttSrv, err := uringnet.Listen("tcp", ":1883", broker.Handler())
	if err != nil {
		log.Fatal(err)
	}
	go mqttSrv.Serve()

	gw, err := gateway.New(gateway.Config{Broker: broker, Retain: true, OnlyChanges: true,
		Poller: modbus.PollerConfig{Interval: 10 * time.Second}})
	if err != nil {
		log.Fatal(err)
	}
	meter := []modbus.Point{
		{Name: "voltage", Table: modbus.InputRegister, Address: 0, Type: modbus.Float32, Order: modbus.CDAB},
		{Name: "current", Table: modbus.InputRegister, Address: 2, Scale: 0.01},
		{Name: "energy", Table: modbus.InputRegister, Address: 3, Type: modbus.Uint32, Scale: 0.01, Interval: time.Minute},
	}
	var dtu *modbus.DTUServer
	dtu = modbus.NewDTUServer(modbus.DTUConfig{
		LinkConfig: modbus.LinkConfig{Framing: modbus.RTUOverTCP, Timeout: 2 * time.Second, Retries: 1},
		Register:   modbus.RegisterFixed(15),
		OnOnline: func(l *modbus.Link) {
			id := l.ID()
			gw.Poller().Add(modbus.Device{Name: id, Points: meter, Link: func() *modbus.Link { return dtu.Link(id) }})
		},
	})
	dtuSrv, err := uringnet.Listen("tcp", ":9000", dtu.Handler())
	if err != nil {
		log.Fatal(err)
	}
	dtuSrv.Serve()
}
