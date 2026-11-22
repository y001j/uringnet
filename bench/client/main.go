// echo 压测客户端：c 个长连接，每个连接循环「写 p 个 s 字节的请求 → 读回全部响应」。
// 输出 QPS、客户端 CPU 占用，以及按 1/16 采样的往返延迟 p50/p99。
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "server address")
	conns := flag.Int("c", 64, "number of connections")
	size := flag.Int("s", 128, "request size in bytes")
	dur := flag.Duration("d", 5*time.Second, "measurement duration")
	depth := flag.Int("p", 1, "pipeline depth: requests in flight per connection")
	timeout := flag.Duration("timeout", 30*time.Second, "per-round-trip timeout; a connection that stalls longer stops")
	flag.Parse()
	var ops atomic.Int64
	var latMu sync.Mutex
	var lats []time.Duration
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < *conns; i++ {
		c, err := net.Dial("tcp4", *addr)
		if err != nil {
			panic(err)
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			msg := make([]byte, *size**depth)
			buf := make([]byte, *size**depth)
			for k := 0; ; k++ {
				select {
				case <-stop:
					c.Close()
					return
				default:
				}
				var t0 time.Time
				if k&15 == 0 {
					t0 = time.Now()
				}
				c.SetDeadline(time.Now().Add(*timeout))
				if _, err := c.Write(msg); err != nil {
					return
				}
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				ops.Add(int64(*depth))
				if k&15 == 0 {
					latMu.Lock()
					lats = append(lats, time.Since(t0))
					latMu.Unlock()
				}
			}
		}(c)
	}
	var ru0 syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
	opsStart := ops.Load()
	time.Sleep(*dur)
	var ru1 syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
	ops0 := ops.Load() - opsStart
	close(stop)
	wg.Wait()
	q := float64(ops0) / dur.Seconds()
	cpu := func(r syscall.Rusage) float64 {
		return float64(r.Utime.Nano()+r.Stime.Nano()) / 1e9
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	pct := func(p float64) float64 {
		if len(lats) == 0 {
			return 0
		}
		return float64(lats[int(float64(len(lats)-1)*p)].Microseconds())
	}
	fmt.Printf("c=%d p=%d s=%d qps=%.0f clientcpu=%.0f%% p50=%.0fus p99=%.0fus\n", *conns, *depth, *size, q, (cpu(ru1)-cpu(ru0))/dur.Seconds()*100, pct(0.5), pct(0.99))
}
