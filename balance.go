//go:build linux
// +build linux

package uringnet

import (
	"hash/fnv"

	"golang.org/x/sys/unix"
)

// LoadBalancing selects how new connections are spread over the rings (WithLoadBalancing).
type LoadBalancing int

const (
	// BalanceKernel gives each ring its own SO_REUSEPORT listening socket and lets the kernel hash
	// new connections over them (the default, and the cheapest: no handoff between rings).
	BalanceKernel LoadBalancing = iota
	// BalanceRoundRobin assigns new connections to the rings in turn.
	BalanceRoundRobin
	// BalanceLeastConnections assigns each new connection to the ring with the fewest connections.
	BalanceLeastConnections
	// BalanceSourceAddrHash assigns connections by a hash of the client's IP address, so all
	// connections from one client share a ring (unix sockets fall back to round robin).
	BalanceSourceAddrHash
)

// WithLoadBalancing selects how new connections are spread over the rings (default BalanceKernel).
// The other strategies use one listening socket; ring 0 accepts and hands each connection to the
// chosen ring, which costs a cross-thread handoff per connection but balances long-lived
// connections evenly.
func WithLoadBalancing(lb LoadBalancing) Option { return func(o *options) { o.balance = lb } }

// balancer picks the ring for a newly accepted connection. It is shared by a server's rings and only
// used by the accepting ring's loop thread, except for the per-ring counters, which are atomic.
type balancer struct {
	lb    LoadBalancing
	rings []*URingNet
	next  uint32
}

func (b *balancer) pick(fd int) *URingNet {
	n := uint32(len(b.rings))
	switch b.lb {
	case BalanceLeastConnections:
		best := b.rings[0]
		bestLoad := best.connCount.Load() + best.incoming.Load()
		for _, rn := range b.rings[1:] {
			if load := rn.connCount.Load() + rn.incoming.Load(); load < bestLoad {
				best, bestLoad = rn, load
			}
		}
		return best
	case BalanceSourceAddrHash:
		if ip := peerIP(fd); ip != nil {
			h := fnv.New32a()
			h.Write(ip)
			return b.rings[h.Sum32()%n]
		}
	}
	b.next++
	return b.rings[b.next%n]
}

func peerIP(fd int) []byte {
	sa, err := unix.Getpeername(fd)
	if err != nil {
		return nil
	}
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		return a.Addr[:]
	case *unix.SockaddrInet6:
		return a.Addr[:]
	}
	return nil
}

// acceptedConn opens an accepted connection on this ring, or hands it to the ring the balancer picks.
func (rn *URingNet) acceptedConn(fd int) {
	if rn.balancer == nil {
		rn.openConn(fd, nil, rn.tlsServer, false, nil)
		return
	}
	target := rn.balancer.pick(fd)
	if target == rn {
		rn.openConn(fd, nil, rn.tlsServer, false, nil)
		return
	}
	target.incoming.Add(1) // counted until opened, so a burst does not all go to the same ring
	err := target.Post(func() {
		target.incoming.Add(-1)
		if target.stopping || target.draining {
			unix.Close(fd)
			return
		}
		target.openConn(fd, nil, target.tlsServer, false, nil)
	})
	if err != nil {
		target.incoming.Add(-1)
		unix.Close(fd)
	}
}
