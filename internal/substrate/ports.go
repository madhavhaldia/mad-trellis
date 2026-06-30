package substrate

import (
	"fmt"
	"net"
	"sync"
)

// portAllocator hands out disjoint host TCP port blocks — the "ports" slice of
// the forkable surface (Inv 1: per-agent PORT allocation, not just service
// URLs). It lives inside the single arbiter daemon (Inv 5), so a process-wide
// reserved set DETERMINISTICALLY guarantees that no two live boundaries are
// handed the same port. Each candidate is also OS-probed free at allocation
// time.
//
// Residual (chafe, documented seam): between allocation and the agent actually
// binding, an EXTERNAL process could grab a freed port (TOCTOU) — a plain host
// has one port namespace. True per-agent port namespacing is the CONTAINER grain
// (Inv 10-grainswap), where every agent may bind :8080 in its own netns. v1
// worktree grain guarantees cross-boundary disjointness, which is what Inv 1
// requires of US; it does not own the whole host's port table.
type portAllocator struct {
	mu       sync.Mutex
	reserved map[int]struct{}
}

func newPortAllocator() *portAllocator {
	return &portAllocator{reserved: map[int]struct{}{}}
}

// allocate reserves n distinct host ports not held by any other live boundary.
// It keeps every probe listener OPEN until all n are gathered, so the OS will
// not return the same ephemeral port twice within the call; cross-boundary
// disjointness is then guaranteed by the reserved set. The probes are closed
// before returning (the agent, not us, binds them).
func (a *portAllocator) allocate(n int) ([]int, error) {
	if n <= 0 {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	var probes []net.Listener
	defer func() {
		for _, ln := range probes {
			_ = ln.Close()
		}
	}()

	ports := make([]int, 0, n)
	for len(ports) < n {
		if len(probes) > n+256 {
			return nil, fmt.Errorf("substrate: could not allocate %d free ports (too many collisions)", n)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("substrate: port probe: %w", err)
		}
		probes = append(probes, ln) // hold open so the OS won't repeat this port
		p := ln.Addr().(*net.TCPAddr).Port
		if _, taken := a.reserved[p]; taken {
			continue // reserved by another live boundary; keep probing
		}
		a.reserved[p] = struct{}{}
		ports = append(ports, p)
	}
	return ports, nil
}

// release returns ports to the pool. Idempotent and safe on an unknown port.
func (a *portAllocator) release(ports []int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range ports {
		delete(a.reserved, p)
	}
}

// reservedCount is a test/diagnostic accessor for the number of live reservations.
func (a *portAllocator) reservedCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.reserved)
}
