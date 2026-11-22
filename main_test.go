//go:build linux
// +build linux

package uringnet

import (
	"os"
	"testing"
)

// URINGNET_BACKEND=epoll (or io_uring) makes all tests use that backend; CI runs both.
func TestMain(m *testing.M) {
	switch os.Getenv("URINGNET_BACKEND") {
	case "epoll":
		defaultBackend = BackendEpoll
	case "io_uring", "iouring":
		defaultBackend = BackendIOUring
	}
	os.Exit(m.Run())
}
