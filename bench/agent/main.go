// 跨机器压测的代理：和 echo 服务一起运行在服务端机器上，通过 HTTP 向远端客户端报告
// 各服务的端口和已用 CPU 时间，客户端据此计算每个测试期间的服务端 CPU 占用。
//
//	agent -addr :9099 epoll=1234:9101 uringnet-best=1240:9105 ...
//
// 接口：
//
//	GET /info            服务端机器信息：kernel=... cpus=... host=...
//	GET /servers         每行 "<名称> <端口>"
//	GET /cpu?name=<名称>  "<CPU 秒数> <代理单调时钟秒数>"，两次读数相减即得 CPU 占用
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type server struct {
	name string
	pid  int
	port int
}

// 内核 USER_HZ：/proc/<pid>/stat 中 utime、stime 的单位，Linux 上固定为 100
const userHZ = 100

func main() {
	addr := flag.String("addr", ":9099", "listen address")
	flag.Parse()
	var servers []server
	byName := map[string]server{}
	for _, arg := range flag.Args() {
		name, rest, ok1 := strings.Cut(arg, "=")
		pidStr, portStr, ok2 := strings.Cut(rest, ":")
		pid, err1 := strconv.Atoi(pidStr)
		port, err2 := strconv.Atoi(portStr)
		if !ok1 || !ok2 || err1 != nil || err2 != nil {
			log.Fatalf("bad argument %q, want name=pid:port", arg)
		}
		s := server{name, pid, port}
		servers = append(servers, s)
		byName[name] = s
	}
	start := time.Now()

	http.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		kernel, _ := os.ReadFile("/proc/sys/kernel/osrelease")
		host, _ := os.Hostname()
		fmt.Fprintf(w, "kernel=%s cpus=%d host=%s\n", strings.TrimSpace(string(kernel)), runtime.NumCPU(), host)
	})
	http.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		for _, s := range servers {
			fmt.Fprintf(w, "%s %d\n", s.name, s.port)
		}
	})
	http.HandleFunc("/cpu", func(w http.ResponseWriter, r *http.Request) {
		s, ok := byName[r.URL.Query().Get("name")]
		if !ok {
			http.Error(w, "unknown server", http.StatusNotFound)
			return
		}
		secs, err := cpuSeconds(s.pid)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "%.3f %.6f\n", secs, time.Since(start).Seconds())
	})
	log.Printf("agent listening on %s, %d servers", *addr, len(servers))
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// cpuSeconds 读取进程的用户态 + 内核态 CPU 时间（秒）
func cpuSeconds(pid int) (float64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// 进程名（第 2 个字段）可能含空格，从最后一个 ')' 之后开始数：其后第 12、13 个字段是 utime、stime
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 13 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	ut, _ := strconv.ParseFloat(f[11], 64)
	st, _ := strconv.ParseFloat(f[12], 64)
	return (ut + st) / userHZ, nil
}
