module github.com/y001j/UringNet

go 1.19

replace uring => ./uring

require (
	github.com/hodgesds/iouring-go v0.0.0-20201011003013-bf9ad57e7a1d
	github.com/panjf2000/gnet/v2 v2.3.3
	golang.org/x/sys v0.12.0
	uring v0.0.0
)

require (
	github.com/pkg/errors v0.9.1 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	go.uber.org/atomic v1.7.0 // indirect
	go.uber.org/multierr v1.6.0 // indirect
	go.uber.org/zap v1.21.0 // indirect
	golang.org/x/sync v0.3.0 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)
