//go:build linux
// +build linux

package uringnet

import (
	"os"
	"testing"
)

// URINGNET_BACKEND=epoll（或 io_uring）让所有测试使用指定的后端；CI 两种都跑
func TestMain(m *testing.M) {
	switch os.Getenv("URINGNET_BACKEND") {
	case "epoll":
		defaultBackend = BackendEpoll
	case "io_uring", "iouring":
		defaultBackend = BackendIOUring
	}
	os.Exit(m.Run())
}
