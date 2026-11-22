module github.com/y001j/uringnet/bench

go 1.21

require (
	github.com/panjf2000/gnet/v2 v2.3.3
	github.com/y001j/iouring-go v0.1.0
	github.com/y001j/uringnet v0.0.0
	golang.org/x/sys v0.30.0
)

require (
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	go.uber.org/atomic v1.7.0 // indirect
	go.uber.org/multierr v1.6.0 // indirect
	go.uber.org/zap v1.21.0 // indirect
	golang.org/x/sync v0.3.0 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)

replace github.com/y001j/uringnet => ../
