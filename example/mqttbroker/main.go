// An MQTT 3.1.1 broker on uringnet. Devices connect on :1883; every message they publish is also
// passed to the application (here: logged), and the application can publish to devices.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/y001j/uringnet"
	"github.com/y001j/uringnet/mqtt"
)

func main() {
	addr := flag.String("addr", ":1883", "listen address")
	verbose := flag.Bool("v", false, "log every published message")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	broker := mqtt.NewBroker(mqtt.Config{
		OnPublish: func(m *mqtt.Message) {
			if *verbose {
				log.Info("publish", "client", m.ClientID, "topic", m.Topic, "qos", m.QoS, "bytes", len(m.Payload))
			}
		},
		OnConnect:    func(id string) { log.Info("connected", "client", id) },
		OnDisconnect: func(id string, err error) { log.Info("disconnected", "client", id, "err", err) },
	})
	srv, err := uringnet.Listen("tcp", *addr, broker.Handler(),
		uringnet.WithTCPKeepAlive(time.Minute),
		uringnet.WithLogger(log))
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	log.Info("MQTT broker listening", "addr", srv.Addr(), "backend", srv.Backend())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		broker.Close()
	}()
	if err := srv.Serve(); !errors.Is(err, uringnet.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
