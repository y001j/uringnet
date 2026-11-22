//go:build linux
// +build linux

package mqtt_test

import (
	"log"
	"time"

	"github.com/y001j/uringnet"
	"github.com/y001j/uringnet/mqtt"
)

// A broker for devices on :1883. The application sees every message the devices publish and
// sends commands back with Publish.
func ExampleBroker() {
	broker := mqtt.NewBroker(mqtt.Config{
		SessionExpiry: 24 * time.Hour,
		Authenticate: func(clientID, username string, password []byte) bool {
			return username == "device" && string(password) == "secret"
		},
		OnPublish: func(m *mqtt.Message) {
			log.Printf("%s published %d bytes to %s", m.ClientID, len(m.Payload), m.Topic)
		},
	})
	srv, err := uringnet.Listen("tcp", ":1883", broker.Handler())
	if err != nil {
		log.Fatal(err)
	}
	go srv.Serve()

	// Send a command to every device subscribed to its command topic
	broker.Publish("devices/all/cmd", []byte("report"), 1, false)

	// With MQTT 5 properties and an expiry: devices that are offline for longer never see it
	broker.PublishMessage(&mqtt.Message{Topic: "devices/42/cmd", Payload: []byte(`{"op":"reboot"}`), QoS: 1,
		Expiry: time.Minute, Properties: &mqtt.Properties{ContentType: "application/json"}})
}
