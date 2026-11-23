//go:build linux
// +build linux

// Command gateway is an IoT gateway in one process: an MQTT broker, a DTU server and Modbus
// devices polled by a point table, with the values published to MQTT.
//
//	gateway -config gateway.json
//
// See gateway.json for the configuration. Values appear on <prefix>/<device>/<point>; with
// "writes": true, publishing to <prefix>/<device>/<point>/set writes the point.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/y001j/uringnet"
	"github.com/y001j/uringnet/gateway"
	"github.com/y001j/uringnet/modbus"
	"github.com/y001j/uringnet/mqtt"
)

type config struct {
	MQTT struct {
		Listen string `json:"listen"`
	} `json:"mqtt"`
	Gateway struct {
		Prefix      string `json:"prefix"`
		Interval    string `json:"interval"`
		Layout      string `json:"layout"` // "point" or "device"
		Retain      bool   `json:"retain"`
		OnlyChanges bool   `json:"only_changes"`
		Writes      bool   `json:"writes"`
		MaxGap      int    `json:"max_gap"`
	} `json:"gateway"`
	DTU *struct {
		Listen    string `json:"listen"`
		Framing   string `json:"framing"`
		Register  int    `json:"register"` // registration packet length; 0: none
		Heartbeat string `json:"heartbeat"`
		Timeout   string `json:"timeout"`
		Retries   int    `json:"retries"`
		// Auto polls DTUs that are not in "devices" with a template, named by their ID
		Auto *struct {
			Template string `json:"template"`
			Unit     byte   `json:"unit"`
		} `json:"auto"`
	} `json:"dtu"`
	Client struct {
		Timeout string `json:"timeout"`
		Retries int    `json:"retries"`
	} `json:"client"`
	Templates map[string][]modbus.Point `json:"templates"`
	Devices   []struct {
		Name     string         `json:"name"`
		DTU      string         `json:"dtu"`     // a DTU's ID, or
		Address  string         `json:"address"` // host:port of a device to connect to
		Framing  string         `json:"framing"` // "tcp" (default) or "rtu", for address
		Unit     byte           `json:"unit"`
		Template string         `json:"template"`
		Points   []modbus.Point `json:"points"`
	} `json:"devices"`
}

func duration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Fatalf("invalid duration %q: %v", s, err)
	}
	return d
}

func framing(s string) modbus.Framing {
	switch strings.ToLower(s) {
	case "", "tcp":
		return modbus.TCP
	case "rtu":
		return modbus.RTUOverTCP
	}
	log.Fatalf("invalid framing %q (tcp or rtu)", s)
	return 0
}

func main() {
	path := flag.String("config", "gateway.json", "configuration file")
	flag.Parse()
	var cfg config
	b, err := os.ReadFile(*path)
	if err != nil {
		log.Fatal(err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Fatalf("%s: %v", *path, err)
	}
	if cfg.MQTT.Listen == "" {
		cfg.MQTT.Listen = ":1883"
	}

	// MQTT broker
	broker := mqtt.NewBroker(mqtt.Config{SessionExpiry: 24 * time.Hour})
	mqttSrv, err := uringnet.Listen("tcp", cfg.MQTT.Listen, broker.Handler(), uringnet.WithTCPKeepAlive(time.Minute))
	if err != nil {
		log.Fatal(err)
	}
	go mqttSrv.Serve()
	log.Printf("MQTT broker on %s", mqttSrv.Addr())

	// Gateway
	layout := gateway.PerPoint
	if cfg.Gateway.Layout == "device" {
		layout = gateway.PerDevice
	}
	gw, err := gateway.New(gateway.Config{
		Broker: broker, Prefix: cfg.Gateway.Prefix, Layout: layout, Retain: cfg.Gateway.Retain,
		OnlyChanges: cfg.Gateway.OnlyChanges, Writes: cfg.Gateway.Writes,
		Poller: modbus.PollerConfig{Interval: duration(cfg.Gateway.Interval, 5*time.Second), MaxGap: cfg.Gateway.MaxGap},
	})
	if err != nil {
		log.Fatal(err)
	}
	poller := gw.Poller()

	points := func(name, template string, own []modbus.Point) []modbus.Point {
		if template == "" {
			return own
		}
		t, ok := cfg.Templates[template]
		if !ok {
			log.Fatalf("device %s: unknown template %q", name, template)
		}
		return append(append([]modbus.Point(nil), t...), own...)
	}

	// DTU server
	var dtu *modbus.DTUServer
	configured := map[string]bool{} // DTU IDs listed in "devices"
	for _, d := range cfg.Devices {
		if d.DTU != "" {
			configured[d.DTU] = true
		}
	}
	if cfg.DTU != nil {
		dc := modbus.DTUConfig{
			LinkConfig: modbus.LinkConfig{Framing: framing(cfg.DTU.Framing), Timeout: duration(cfg.DTU.Timeout, 2*time.Second), Retries: cfg.DTU.Retries},
			Heartbeat:  []byte(cfg.DTU.Heartbeat),
			OnOnline:   func(l *modbus.Link) { log.Printf("DTU %s online", l.ID()) },
			OnOffline:  func(l *modbus.Link, err error) { log.Printf("DTU %s offline: %v", l.ID(), err) },
		}
		if cfg.DTU.Register > 0 {
			dc.Register = modbus.RegisterFixed(cfg.DTU.Register)
		}
		if auto := cfg.DTU.Auto; auto != nil {
			pts := points("dtu auto", auto.Template, nil)
			dc.OnOnline = func(l *modbus.Link) {
				id := l.ID()
				log.Printf("DTU %s online", id)
				if configured[id] {
					return
				}
				// Added once; it stays (reported offline) when the DTU goes away
				err := poller.Add(modbus.Device{Name: id, Unit: auto.Unit, Points: pts,
					Link: func() *modbus.Link { return dtu.Link(id) }})
				if err == nil {
					log.Printf("DTU %s added with template %s", id, auto.Template)
				}
			}
		}
		dtu = modbus.NewDTUServer(dc)
		dtuSrv, err := uringnet.Listen("tcp", cfg.DTU.Listen, dtu.Handler(), uringnet.WithTCPKeepAlive(time.Minute))
		if err != nil {
			log.Fatal(err)
		}
		go dtuSrv.Serve()
		defer dtuSrv.Close()
		log.Printf("DTU server on %s (%v)", dtuSrv.Addr(), dc.Framing)
	}

	// Devices
	clients := map[modbus.Framing]*modbus.Client{}
	for _, d := range cfg.Devices {
		dev := modbus.Device{Name: d.Name, Unit: d.Unit, Points: points(d.Name, d.Template, d.Points)}
		switch {
		case d.DTU != "":
			if dtu == nil {
				log.Fatalf("device %s: a DTU device needs the \"dtu\" section", d.Name)
			}
			id := d.DTU
			dev.Link = func() *modbus.Link { return dtu.Link(id) }
		case d.Address != "":
			f := framing(d.Framing)
			if clients[f] == nil {
				c, err := modbus.NewClient(modbus.ClientConfig{LinkConfig: modbus.LinkConfig{
					Framing: f, Timeout: duration(cfg.Client.Timeout, time.Second), Retries: cfg.Client.Retries}})
				if err != nil {
					log.Fatal(err)
				}
				defer c.Close()
				clients[f] = c
			}
			dev.Link = modbus.LinkOf(clients[f].Dial(d.Address))
		default:
			log.Fatalf("device %s: needs \"dtu\" or \"address\"", d.Name)
		}
		if err := poller.Add(dev); err != nil {
			log.Fatal(err)
		}
	}
	prefix := cfg.Gateway.Prefix
	if prefix == "" {
		prefix = "devices"
	}
	fmt.Printf("polling %d configured devices; values on %s/<device>/<point>\n", len(cfg.Devices), prefix)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	gw.Close()
	mqttSrv.Close()
	broker.Close()
}
