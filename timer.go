//go:build linux
// +build linux

package uringnet

import (
	"syscall"
	"time"
)

// Timeouts are checked every quarter of the shortest timeout, within [minTick, maxTick], so a
// connection is closed between 1 and 1.25 times the timeout after its last activity.
const (
	minTick = 5 * time.Millisecond
	maxTick = time.Second
)

// setupTimeouts derives the tick interval from IdleTimeout/WriteTimeout; called from start.
func (rn *URingNet) setupTimeouts() {
	shortest := time.Duration(0)
	for _, d := range []time.Duration{rn.IdleTimeout, rn.WriteTimeout} {
		if d > 0 && (shortest == 0 || d < shortest) {
			shortest = d
		}
	}
	if shortest == 0 {
		return
	}
	rn.tickEvery = shortest / 4
	if rn.tickEvery < minTick {
		rn.tickEvery = minTick
	} else if rn.tickEvery > maxTick {
		rn.tickEvery = maxTick
	}
	// Activity happens anywhere within a tick, so count one extra tick to make sure the full timeout has passed
	ticks := func(d time.Duration) uint64 {
		if d <= 0 {
			return 0
		}
		return uint64((d+rn.tickEvery-1)/rn.tickEvery) + 1
	}
	rn.idleTicks, rn.writeTicks = ticks(rn.IdleTimeout), ticks(rn.WriteTimeout)
}

// startTimeouts starts the timeout checks on the loop thread (posted by RunMany).
func (rn *URingNet) startTimeouts() {
	if rn.tickEvery > 0 {
		rn.after(rn.tickEvery, rn.onTick)
	}
}

// onTick advances the tick and closes connections that timed out.
func (rn *URingNet) onTick() {
	if rn.stopping {
		return
	}
	rn.tick++
	for _, c := range rn.conns {
		if c.closed {
			continue
		}
		if rn.idleTicks > 0 && rn.tick-c.lastActive >= rn.idleTicks {
			rn.timeouts++
			rn.idleCloses++
			rn.log.Debug("uringnet: closing idle connection", "fd", c.fd)
			c.fail(ErrIdleTimeout)
			continue
		}
		if rn.writeTicks > 0 && len(c.q) > 0 && rn.tick-c.lastSend >= rn.writeTicks {
			rn.timeouts++
			rn.writeCloses++
			rn.log.Debug("uringnet: closing connection, send queue stalled", "fd", c.fd, "queued", c.qBytes)
			c.fail(ErrWriteTimeout)
		}
	}
	rn.after(rn.tickEvery, rn.onTick)
}

// after runs fn on the loop thread once d has passed (a one-shot timer). Loop thread only.
// If the event loop closes first, the timer is cancelled and fn does not run.
func (rn *URingNet) after(d time.Duration, fn func()) {
	rn.loop.Timeout(d, func(res int32, _ uint32) {
		if res == -int32(syscall.ETIME) {
			fn()
		}
	})
}
