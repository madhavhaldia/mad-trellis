package integration

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeClock is a controllable Clock so a test can stamp rows at a known time, then
// advance to age them deterministically (no wall-clock flakiness).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newClockedStore(t *testing.T, clk Clock) *store {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "integration.db"), clk)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.close() })
	return st
}

func has(t *testing.T, st *store, branch string) bool {
	t.Helper()
	_, ok, err := st.get(branch)
	if err != nil {
		t.Fatalf("get %s: %v", branch, err)
	}
	return ok
}

// TestGCDeletesAgedTerminalAndAbandoned proves the TTL GC:
//   - DELETES a terminal (approved/withdrawn) row older than terminalRetention;
//   - DELETES an abandoned (requested/changes_requested) row older than
//     abandonedRetention;
//   - NEVER deletes a `claimed` row, however old (the control — it is in-flight);
//   - KEEPS a FRESH `requested` row younger than abandonedRetention (the control
//     proving GC does not nuke a live queue entry just because it is in `requested`).
func TestGCDeletesAgedTerminalAndAbandoned(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &fakeClock{t: base}
	st := newClockedStore(t, clk)

	const terminalRetention = 1 * time.Hour
	const abandonedRetention = 24 * time.Hour

	// --- OLD rows: stamped at base, then we advance well past every retention. ---
	// approved (terminal): request -> claim -> approve.
	_, _ = st.upsertRequest("br-approved", "", "b")
	_, _ = st.claim("br-approved", "i")
	_, _ = st.approve("br-approved", "deadbeef")
	// withdrawn (terminal): request -> cancel.
	_, _ = st.upsertRequest("br-withdrawn", "", "b")
	_, _ = st.cancel("br-withdrawn")
	// changes_requested (abandoned soft-terminal): request -> claim -> reject.
	_, _ = st.upsertRequest("br-changes", "", "b")
	_, _ = st.claim("br-changes", "i")
	_, _ = st.reject("br-changes", "needs work")
	// requested (abandoned): a cold queue entry nobody picked up.
	_, _ = st.upsertRequest("br-abandoned", "", "b")
	// claimed (in-flight): the CONTROL — must NEVER be deleted, however old.
	_, _ = st.upsertRequest("br-claimed", "", "b")
	_, _ = st.claim("br-claimed", "i")

	// Age everything far past 24h so both retentions are exceeded for the old rows.
	clk.advance(48 * time.Hour)

	// --- FRESH `requested` row: stamped AFTER the advance, so it is young. ---
	_, _ = st.upsertRequest("br-fresh", "", "b")

	n, err := st.gc(clk.Now(), terminalRetention, abandonedRetention)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	// Deleted: br-approved, br-withdrawn, br-changes, br-abandoned == 4.
	if n != 4 {
		t.Fatalf("gc must delete exactly the 4 aged terminal+abandoned rows; got %d", n)
	}

	if has(t, st, "br-approved") {
		t.Fatal("an aged approved (terminal) row must be GC'd")
	}
	if has(t, st, "br-withdrawn") {
		t.Fatal("an aged withdrawn (terminal) row must be GC'd")
	}
	if has(t, st, "br-changes") {
		t.Fatal("an aged changes_requested (abandoned) row must be GC'd")
	}
	if has(t, st, "br-abandoned") {
		t.Fatal("an aged requested (abandoned) row must be GC'd")
	}
	// CONTROL: a claimed row is in-flight and must survive even though it is ancient.
	if !has(t, st, "br-claimed") {
		t.Fatal("a claimed (in-flight) row must NEVER be GC'd, however old")
	}
	if got, _, _ := st.get("br-claimed"); got.State != StateClaimed {
		t.Fatalf("the claimed row must remain claimed; got %q", got.State)
	}
	// CONTROL: a fresh requested row (younger than abandonedRetention) is a live queue
	// entry and must survive — GC must not nuke the integrator's pending work.
	if !has(t, st, "br-fresh") {
		t.Fatal("a fresh requested row younger than abandonedRetention must be KEPT")
	}
}

// TestGCTerminalRetentionBoundary proves the terminal vs abandoned retentions are
// applied to the RIGHT state classes: a terminal row older than terminalRetention
// but younger than abandonedRetention is deleted, while an abandoned row of the SAME
// age is KEPT (because the long abandonedRetention has not elapsed). This pins that
// requested/changes_requested get the LONG grace — a builder requesting then exiting
// for async review is normal and must not be reaped on the short terminal TTL.
func TestGCTerminalRetentionBoundary(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &fakeClock{t: base}
	st := newClockedStore(t, clk)

	const terminalRetention = 1 * time.Hour
	const abandonedRetention = 24 * time.Hour

	// approved (terminal) and requested (abandoned), stamped together at base.
	_, _ = st.upsertRequest("br-approved", "", "b")
	_, _ = st.claim("br-approved", "i")
	_, _ = st.approve("br-approved", "x")
	_, _ = st.upsertRequest("br-requested", "", "b")

	// Advance to 2h: past terminalRetention (1h), well short of abandonedRetention (24h).
	clk.advance(2 * time.Hour)

	n, err := st.gc(clk.Now(), terminalRetention, abandonedRetention)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if n != 1 {
		t.Fatalf("only the terminal row crossed its (short) retention; got %d deleted", n)
	}
	if has(t, st, "br-approved") {
		t.Fatal("a terminal row older than terminalRetention must be GC'd")
	}
	// CONTROL: the abandoned row of the SAME age survives — the long grace is in force.
	if !has(t, st, "br-requested") {
		t.Fatal("an abandoned row younger than the LONG abandonedRetention must be KEPT")
	}
}
