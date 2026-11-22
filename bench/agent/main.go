// Agent for cross-machine benchmarks: runs next to the echo servers on the server machine and reports over HTTP
// each server's port and CPU time used, from which the remote client computes server CPU per test.
//
//	agent -addr :9099 epoll=1234:9101 uringnet-best=1240:9105 ...
//
// Endpoints:
//
//	GET /info            server machine: kernel=... cpus=... host=...
//	GET /servers         one "<name> <port>" per line
//	GET /cpu?name=<name>  "<CPU seconds> <agent monotonic seconds>"; the difference of two readings gives CPU usage
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

// Kernel USER_HZ: the unit of utime and stime in /proc/<pid>/stat, always 100 on Linux.
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

// cpuSeconds reads a process's user + system CPU time in seconds.
func cpuSeconds(pid int) (float64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The process name (field 2) may contain spaces, so count from after the last ')': fields 12 and 13 after it are utime and stime
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
