//go:build linux
// +build linux

package uringnet

import "time"

// clockBase anchors monoNow, a monotonic clock in nanoseconds that fits an atomic int64.
var clockBase = time.Now()

func monoNow() int64 { return int64(time.Since(clockBase)) }

// cbBegin marks the start of a user callback on the loop thread (WithSlowCallbackWarning). It returns
// the start time to pass to cbEnd, 0 when detection is off.
func (rn *URingNet) cbBegin() int64 {
	if rn.slowCallback <= 0 {
		return 0
	}
	t := monoNow()
	rn.cbStart.Store(t)
	return t
}

// cbEnd marks the end of a callback started with cbBegin and reports it if it was slow.
func (rn *URingNet) cbEnd(t0 int64, callback string, fd int) {
	if t0 == 0 {
		return
	}
	rn.cbStart.Store(0)
	if d := time.Duration(monoNow() - t0); d >= rn.slowCallback {
		rn.slowCallbacks++
		rn.log.Warn("uringnet: slow callback blocked the event loop", "callback", callback, "fd", fd, "ring", rn.index, "took", d)
	}
}

// watchCallbacks reports callbacks that are still running after the threshold, which cbEnd cannot do
// for a callback that never returns. It runs on its own goroutine until stop is closed.
func (rn *URingNet) watchCallbacks(stop <-chan struct{}) {
	every := rn.slowCallback / 2
	if every < 10*time.Millisecond {
		every = 10 * time.Millisecond
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var reported int64
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		start := rn.cbStart.Load()
		if start == 0 || start == reported {
			continue
		}
		if d := time.Duration(monoNow() - start); d >= rn.slowCallback {
			reported = start
			rn.log.Warn("uringnet: callback still running, the event loop is blocked", "ring", rn.index, "running_for", d)
		}
	}
}
