package integrator

// A REAL lease.Ledger adapter (test-only) so single-writer exclusion is proven
// against the genuine SQLite CAS, not a mock — confirming "single-writer is the
// LEASE, not a private mutex". Importing lease here is acyclic: lease never
// imports the integrator.

import (
	"testing"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/lease"
)

type realLeaseGate struct{ l *lease.Ledger }

func newRealLeaseGate(t *testing.T) *realLeaseGate {
	t.Helper()
	l, err := lease.Open("", nil) // in-memory ledger
	if err != nil {
		t.Fatalf("lease open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return &realLeaseGate{l}
}

func (g *realLeaseGate) Acquire(key []byte, holder string, ttl time.Duration) (bool, string, error) {
	res, err := g.l.Acquire(key, holder, ttl)
	if err != nil {
		return false, "", err
	}
	return res.Granted, res.Holder, nil
}

func (g *realLeaseGate) Release(key []byte, holder string) (bool, error) {
	return g.l.Release(key, holder)
}
