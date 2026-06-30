package liveness

// Hand-authored invariant suite for project 8 (liveness-recovery) — the contract,
// authored by hand (REVIEW-GATED). Each owned clause → a proving test:
//
//   3-reclaim single-death progress     → TestReclaimExpiredLease
//   NO-FALSE-POSITIVE (two layers)       → TestNoFalsePositiveReclaim (+control)
//   re-acquirer-safe re-fire             → TestReclaimDoesNotStealFreshReacquirer
//   never-mutate (detector+trigger only) → TestNeverMutate (+control)
//   mid-integration abort                → TestMidIntegrationAbort (+control)
//   restart reattachment                 → TestRestartReattachment
//   no-dispatch (frees, never resumes)   → TestLivenessNeverSpawns

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/lease"
)

// --- real-ledger adapter + manual clock --------------------------------------

type realReclaimer struct{ l *lease.Ledger }

func (a realReclaimer) ExpiredLeases() ([]ExpiredLease, error) {
	infos, err := a.l.ExpiredLeases()
	if err != nil {
		return nil, err
	}
	out := make([]ExpiredLease, 0, len(infos))
	for _, i := range infos {
		out = append(out, ExpiredLease{Key: i.Key, Holder: i.Holder})
	}
	return out, nil
}

func (a realReclaimer) LiveHolders() ([]string, error) {
	infos, err := a.l.ListHolders()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.Holder)
	}
	return out, nil
}

func (a realReclaimer) ReclaimIfExpired(key []byte) (bool, string, error) {
	res, err := a.l.ReclaimIfExpired(key)
	if err != nil {
		return false, "", err
	}
	return res.Reclaimed, res.PriorHolder, nil
}

var trunkKey = []byte("mad-substrate:trunk:v1")

// sessionKeyPrefix mirrors the T2 session-liveness lease key namespace
// ("mad-substrate:session:v1:"); the per-session key is the prefix + sessionID.
var sessionKeyPrefix = []byte("mad-substrate:session:v1:")

func sessionKey(id string) []byte { return append(append([]byte{}, sessionKeyPrefix...), id...) }

type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *manualClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func openLedger(t *testing.T, path string, clk lease.Clock) *lease.Ledger {
	t.Helper()
	l, err := lease.Open(path, clk)
	if err != nil {
		t.Fatalf("lease open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// --- tests -------------------------------------------------------------------

// [3-reclaim] kill a lease-holder (it stops renewing) → its lease expires → Scan
// reclaims it → the resource is re-acquirable. No operator action; progress made.
func TestReclaimExpiredLease(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	key := []byte("k1")
	if res, _ := l.Acquire(key, "dead-A", 100*time.Millisecond); !res.Granted {
		t.Fatal("setup acquire must grant")
	}
	clk.advance(250 * time.Millisecond) // A "dies": never renews; the lease expires

	r, _ := New(realReclaimer{l}, nil, nil, nil, nil)
	rep, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reclaimed != 1 {
		t.Fatalf("the expired lease must be reclaimed; got %+v", rep)
	}
	// Re-acquirable by a new holder — progress made, no operator action.
	if res, _ := l.Acquire(key, "new-B", time.Minute); !res.Granted {
		t.Fatal("after reclaim the lease must be re-acquirable")
	}
}

// NO-FALSE-POSITIVE — the cardinal hazard. TWO independent layers:
//
//	(1) detector: a still-LIVE lease is not in ExpiredLeases, so Scan never even
//	    tries to reclaim it (Reclaimed=0; the holder keeps the lease);
//	(2) ledger guard: even a DIRECT ReclaimIfExpired on a live lease no-ops.
//
// +control: once expired, the SAME lease IS reclaimed (the no-reclaim is not vacuous).
func TestNoFalsePositiveReclaim(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	key := []byte("k1")
	l.Acquire(key, "live-A", time.Hour) // a slow-but-LIVE holder

	// Layer 1: the detector does not surface a live lease.
	if exp, _ := l.ExpiredLeases(); len(exp) != 0 {
		t.Fatalf("a live lease must NOT appear as expired; got %d", len(exp))
	}
	r, _ := New(realReclaimer{l}, nil, nil, nil, nil)
	if rep, _ := r.Scan(); rep.Reclaimed != 0 {
		t.Fatalf("Scan must not reclaim a live lease; got %+v", rep)
	}
	// Layer 2: even a direct reclaim of the live lease no-ops.
	if res, _ := l.ReclaimIfExpired(key); res.Reclaimed {
		t.Fatal("ReclaimIfExpired must NOT free a still-live lease (ledger guard)")
	}
	if info, _, _ := l.Inspect(key); info.Holder != "live-A" {
		t.Fatalf("the live holder must keep its lease; got %q", info.Holder)
	}

	// +control: once expired, the same lease IS reclaimed.
	clk.advance(2 * time.Hour)
	if rep, _ := r.Scan(); rep.Reclaimed != 1 {
		t.Fatalf("control: an expired lease must be reclaimed; got %+v", rep)
	}
}

// re-acquirer-safe: a reclaim in flight must NOT steal a FRESH re-acquirer's
// lease. The detector reads the (then-expired) key, but a new holder acquires it
// before the trigger fires; ReclaimIfExpired then no-ops because the lease is now
// live — the layer-2 guard protects the re-acquirer.
func TestReclaimDoesNotStealFreshReacquirer(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	key := []byte("k1")
	l.Acquire(key, "dead-A", 100*time.Millisecond)
	clk.advance(250 * time.Millisecond) // A's lease is now expired

	rec := realReclaimer{l}
	expired, _ := rec.ExpiredLeases() // detector observes A's key as expired
	if len(expired) != 1 {
		t.Fatalf("expected 1 expired lease; got %d", len(expired))
	}
	// INTERLEAVE: a fresh holder re-acquires the same key before the trigger fires.
	if res, _ := l.Acquire(key, "fresh-B", time.Hour); !res.Granted {
		t.Fatal("fresh re-acquire must grant (the lease is expired)")
	}
	// Now the (stale) trigger fires — it must NOT steal B's live lease.
	reclaimed, _, _ := rec.ReclaimIfExpired(expired[0].Key)
	if reclaimed {
		t.Fatal("reclaim STOLE a fresh re-acquirer's live lease (false-positive)")
	}
	if info, _, _ := l.Inspect(key); info.Holder != "fresh-B" {
		t.Fatalf("the fresh re-acquirer must keep its lease; got %q", info.Holder)
	}
}

// never-mutate: Scan calls ONLY the read (ExpiredLeases) + the sanctioned trigger
// (ReclaimIfExpired) — never a direct table write. The spy exposes a forbidden
// DirectWrite that liveness must never invoke. +control: calling DirectWrite
// directly is recorded, proving the spy is non-vacuous.
func TestNeverMutate(t *testing.T) {
	spy := &spyReclaimer{expired: []ExpiredLease{{Key: []byte("k"), Holder: "A"}}}
	r, _ := New(spy, nil, nil, trunkKey, nil)
	if _, err := r.Scan(); err != nil {
		t.Fatal(err)
	}
	for _, c := range spy.calls {
		if c != "ExpiredLeases" && c != "ReclaimIfExpired" && c != "LiveHolders" {
			t.Fatalf("liveness invoked a non-trigger method %q (must be detector + read + trigger only)", c)
		}
	}
	if spy.directWrites != 0 {
		t.Fatal("liveness must NEVER directly write the lease table")
	}
	// +control: the spy genuinely detects a forbidden direct write.
	spy.DirectWrite()
	if spy.directWrites != 1 {
		t.Fatal("control: the spy must record a direct write (non-vacuity)")
	}
}

type spyReclaimer struct {
	expired      []ExpiredLease
	calls        []string
	directWrites int
}

func (s *spyReclaimer) ExpiredLeases() ([]ExpiredLease, error) {
	s.calls = append(s.calls, "ExpiredLeases")
	return s.expired, nil
}
func (s *spyReclaimer) LiveHolders() ([]string, error) {
	s.calls = append(s.calls, "LiveHolders")
	return nil, nil
}
func (s *spyReclaimer) ReclaimIfExpired(key []byte) (bool, string, error) {
	s.calls = append(s.calls, "ReclaimIfExpired")
	return true, "A", nil
}

// DirectWrite is a FORBIDDEN mutation liveness must never invoke (it is not on
// the LeaseReclaimer interface, so this is belt-and-suspenders).
func (s *spyReclaimer) DirectWrite() { s.directWrites++ }

// mid-integration abort: a `validating` integration whose holder holds NO live
// lease (its promoter is gone) is aborted, and the dead mid-promote holder's
// (TRUNK-lease-expired) boundary is torn down. +control: an integration whose
// holder STILL holds a live lease is NEITHER aborted nor torn down (alive).
func TestMidIntegrationAbort(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	l.Acquire(trunkKey, "dead-A", 100*time.Millisecond) // A held the trunk lease mid-promote
	l.Acquire([]byte("k-C"), "live-C", time.Hour)       // C is alive (holds a live lease)
	clk.advance(250 * time.Millisecond)                 // A's trunk lease expires; C's does not

	integ := &spyIntegrator{inflight: []InFlightIntegration{
		{ID: "int-X", Holder: "dead-A", State: "validating"}, // stuck mid-promote → abort
		{ID: "int-Y", Holder: "live-C", State: "validating"}, // holder alive → must NOT abort
	}}
	bound := &spyBoundary{}
	r, _ := New(realReclaimer{l}, integ, bound, trunkKey, nil)
	rep, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Aborted != 1 || len(integ.aborted) != 1 || integ.aborted[0] != "int-X" {
		t.Fatalf("only the dead holder's integration must be aborted; got aborted=%v", integ.aborted)
	}
	if rep.TornDown != 1 || len(bound.torn) != 1 || bound.torn[0] != "dead-A" {
		t.Fatalf("only the dead mid-promote holder's boundary must be torn down; got %v", bound.torn)
	}
}

// CARDINAL no-false-positive across KEYS (the HIGH regression): a LIVE session
// holds a live trunk lease AND a singular grant. ONLY the singular grant lapses.
// Liveness must reclaim the lapsed grant but must NOT declare the session dead —
// it must NOT abort the session's in-flight integration nor tear down its live
// boundary, because the session still holds a live lease.
func TestNoCrossKeyFalsePositiveOnLiveHolder(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	live := "session-LIVE"
	l.Acquire(trunkKey, live, time.Hour)                                          // trunk lease: LIVE, renewed
	l.Acquire([]byte("mad-substrate:singular:v1:db"), live, 100*time.Millisecond) // a grant that lapses
	clk.advance(250 * time.Millisecond)                                           // ONLY the singular grant expires

	integ := &spyIntegrator{inflight: []InFlightIntegration{
		{ID: "int-LIVE", Holder: live, State: "validating"},
	}}
	bound := &spyBoundary{}
	r, _ := New(realReclaimer{l}, integ, bound, trunkKey, nil)
	rep, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reclaimed != 1 {
		t.Fatalf("the lapsed singular grant must still be reclaimed; got %+v", rep)
	}
	if len(integ.aborted) != 0 {
		t.Fatalf("a LIVE holder's integration must NOT be aborted from a lapsed unrelated key; got %v", integ.aborted)
	}
	if len(bound.torn) != 0 {
		t.Fatalf("a LIVE holder's boundary must NOT be torn down from a lapsed unrelated key; got %v", bound.torn)
	}
	// The session's trunk lease must still be intact (it is alive).
	if info, _, _ := l.Inspect(trunkKey); info.Holder != live || !info.Held {
		t.Fatalf("the live session's trunk lease must remain held; got %+v", info)
	}
}

// T2: a SESSION death — the launcher stopped renewing the session-liveness lease,
// so it expired — with NO other live lease held by that session is dead. Liveness
// reclaims the expired session-liveness lease AND tears down the session's
// boundary, keyed off the session-liveness lease (the canonical session-death
// signal), NOT the trunk lease (this session never promoted). Closes C16/C11: an
// adapter grant under the shared id is also reclaimed once the session is dead.
func TestSessionDeathReclaimsBoundary(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	dead := "s-9-dead"
	// The launcher held the session-liveness lease, then died (stopped renewing).
	l.Acquire(sessionKey(dead), dead, 100*time.Millisecond)
	clk.advance(250 * time.Millisecond) // the session-liveness lease expires → session death

	bound := &spyBoundary{}
	r, _ := NewWithSessionKey(realReclaimer{l}, nil, bound, trunkKey, sessionKeyPrefix, nil)
	rep, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reclaimed != 1 {
		t.Fatalf("the expired session-liveness lease must be reclaimed; got %+v", rep)
	}
	if rep.TornDown != 1 || len(bound.torn) != 1 || bound.torn[0] != dead {
		t.Fatalf("a dead session's boundary must be torn down off the session-liveness lease; got torn=%v", bound.torn)
	}
	if len(rep.DeadHolders) != 1 || rep.DeadHolders[0] != dead {
		t.Fatalf("the dead session must be reported; got %+v", rep.DeadHolders)
	}
}

// C17 REGRESSION (the HIGH-stakes invariant): a LIVE session — its session-liveness
// lease is still renewed — with a LAPSING singular grant must NOT be torn down. The
// lapsed grant is reclaimed, but the session holds a live lease (the session-
// liveness lease), so it is ALIVE: its boundary is never destroyed. This is exactly
// the false-positive death liveness must never commit (Inv 17).
func TestSessionLivenessHoldsBoundaryAliveDespiteLapsedGrant(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	live := "s-10-live"
	l.Acquire(sessionKey(live), live, time.Hour)                                  // session-liveness lease: LIVE, renewed
	l.Acquire([]byte("mad-substrate:singular:v1:db"), live, 100*time.Millisecond) // a supervised grant that lapses
	clk.advance(250 * time.Millisecond)                                           // ONLY the singular grant expires

	bound := &spyBoundary{}
	r, _ := NewWithSessionKey(realReclaimer{l}, nil, bound, trunkKey, sessionKeyPrefix, nil)
	rep, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reclaimed != 1 {
		t.Fatalf("the lapsed singular grant must still be reclaimed; got %+v", rep)
	}
	if len(bound.torn) != 0 {
		t.Fatalf("C17 BREACH: a LIVE session's boundary was torn down from a lapsed unrelated grant; torn=%v", bound.torn)
	}
	// The session-liveness lease is intact (the session is alive).
	if info, _, _ := l.Inspect(sessionKey(live)); info.Holder != live || !info.Held {
		t.Fatalf("the live session's session-liveness lease must remain held; got %+v", info)
	}

	// +control: once the session-liveness lease ALSO lapses (the session truly dies),
	// the SAME boundary IS torn down — proving the no-teardown was a live gate, not a
	// dead path.
	clk.advance(2 * time.Hour)
	rep2, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if rep2.TornDown != 1 || len(bound.torn) != 1 || bound.torn[0] != live {
		t.Fatalf("control: a now-dead session's boundary must be torn down; got torn=%v rep=%+v", bound.torn, rep2)
	}
}

// The session-death signal is DISABLED when no session-key prefix is wired (the
// pre-T2 New constructor): an expired session-liveness lease is still reclaimed
// per-key, but it does NOT trigger a boundary teardown (only the trunk key does).
// This pins that NewWithSessionKey is what opts into the T2 behavior.
func TestSessionDeathIgnoredWithoutSessionKeyWiring(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	dead := "s-11-dead"
	l.Acquire(sessionKey(dead), dead, 100*time.Millisecond)
	clk.advance(250 * time.Millisecond)

	bound := &spyBoundary{}
	r, _ := New(realReclaimer{l}, nil, bound, trunkKey, nil) // no session key wired
	rep, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reclaimed != 1 {
		t.Fatalf("the expired lease is still reclaimed per-key; got %+v", rep)
	}
	if len(bound.torn) != 0 {
		t.Fatalf("without session-key wiring, a session-liveness expiry must NOT tear down a boundary; torn=%v", bound.torn)
	}
}

type spyIntegrator struct {
	inflight []InFlightIntegration
	aborted  []string
	failID   string // Abort of this id returns an error (to test best-effort)
}

func (s *spyIntegrator) InFlight() ([]InFlightIntegration, error) { return s.inflight, nil }
func (s *spyIntegrator) Abort(id string) error {
	if id == s.failID {
		return errors.New("simulated abort error")
	}
	s.aborted = append(s.aborted, id)
	return nil
}

type spyBoundary struct{ torn []string }

func (s *spyBoundary) Teardown(session string) error {
	s.torn = append(s.torn, session)
	return nil
}

// best-effort: a single Abort error must NOT strand the OTHER dead holders' aborts
// or the teardown phase (the death signal was already consumed by reclaiming the
// lease, so a hard return would lose recovery permanently). The pass accumulates
// the error and continues.
func TestBestEffortContinuesPastError(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	l.Acquire(trunkKey, "dead-A", 100*time.Millisecond)
	clk.advance(250 * time.Millisecond)

	integ := &spyIntegrator{
		failID: "int-bad",
		inflight: []InFlightIntegration{
			{ID: "int-bad", Holder: "dead-A", State: "validating"},  // Abort errors
			{ID: "int-good", Holder: "dead-A", State: "validating"}, // must STILL be aborted
		},
	}
	bound := &spyBoundary{}
	r, _ := New(realReclaimer{l}, integ, bound, trunkKey, nil)
	rep, err := r.Scan()
	if err == nil {
		t.Fatal("the accumulated abort error must be surfaced")
	}
	if len(integ.aborted) != 1 || integ.aborted[0] != "int-good" {
		t.Fatalf("a failing abort must not strand the others; got %v", integ.aborted)
	}
	if rep.TornDown != 1 || len(bound.torn) != 1 {
		t.Fatalf("a failing abort must not strand the teardown phase; got torn=%v", bound.torn)
	}
}

// spyClaims is a fake IntegrationClaimReclaimer: it holds a set of sessions that
// currently hold a review claim and, on ReclaimStaleClaims, reverts exactly those
// the live-set death oracle (isDead) reports dead — recording which it reverted so
// the test can assert a LIVE claimer is never touched.
type spyClaims struct {
	claimers  []string
	reclaimed []string
	gcCount   int // returned by GCStale (the TTL-GC count the scan must surface)
	gcCalls   int // how many times GCStale was invoked (proves the scan calls it)
	gcErr     error
}

func (s *spyClaims) ReclaimStaleClaims(isDead func(session string) bool) (int, error) {
	n := 0
	for _, c := range s.claimers {
		if isDead(c) {
			s.reclaimed = append(s.reclaimed, c)
			n++
		}
	}
	return n, nil
}

func (s *spyClaims) GCStale() (int, error) {
	s.gcCalls++
	return s.gcCount, s.gcErr
}

// liveness-bound review claims: a review request stranded in `claimed` by a DEAD
// integrator (a session holding no live lease) is reverted, while one held by a
// LIVE integrator (it holds a live lease, so it is in the `live` set) is NEVER
// reclaimed. Proves the sweep reuses the SAME live set the abort/teardown steps
// consult as its death oracle.
func TestReclaimStaleReviewClaims(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	// "live-integrator" holds a live lease → it is in the live set → ALIVE.
	l.Acquire([]byte("k-live"), "live-integrator", time.Hour)
	// "dead-integrator" holds no lease → not in the live set → DEAD.

	spy := &spyClaims{claimers: []string{"dead-integrator", "live-integrator"}}
	r, _ := NewWithIntegrationReclaim(realReclaimer{l}, nil, nil, trunkKey, sessionKeyPrefix, spy, nil)
	rep, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReclaimedClaims != 1 {
		t.Fatalf("exactly the dead integrator's claim must be reclaimed; got %+v", rep)
	}
	if len(spy.reclaimed) != 1 || spy.reclaimed[0] != "dead-integrator" {
		t.Fatalf("only the dead claimer must be reverted; got %v", spy.reclaimed)
	}
}

// the stale-claim sweep is DISABLED when no IntegrationClaimReclaimer is wired
// (the pre-Wing-3 constructors): Scan runs clean and reports zero reclaimed claims,
// pinning that NewWithIntegrationReclaim is what opts into the behavior.
func TestStaleClaimSweepDisabledWithoutReclaimer(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	r, _ := NewWithSessionKey(realReclaimer{l}, nil, nil, trunkKey, sessionKeyPrefix, nil)
	rep, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReclaimedClaims != 0 {
		t.Fatalf("without a reclaimer wired, no claim sweep must run; got %+v", rep)
	}
}

// liveness-driven GC: each scan invokes the integration plane's GCStale and
// surfaces the deleted count in Report.GCedRecords (and audits when > 0). The GC is
// TTL-driven inside the plane, NOT death-gated, so it runs every pass regardless of
// the live set. Proves the scan wires the GC hook.
func TestScanInvokesGCStale(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	spy := &spyClaims{gcCount: 3}
	r, _ := NewWithIntegrationReclaim(realReclaimer{l}, nil, nil, trunkKey, sessionKeyPrefix, spy, nil)
	rep, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if spy.gcCalls != 1 {
		t.Fatalf("Scan must invoke GCStale exactly once; got %d calls", spy.gcCalls)
	}
	if rep.GCedRecords != 3 {
		t.Fatalf("Scan must surface the GC count; got %+v", rep)
	}
}

// best-effort: a GCStale error is accumulated (surfaced) but does NOT short-circuit
// the pass — the claim sweep still ran and reported its count. +control: with a nil
// error GCStale's count IS surfaced (TestScanInvokesGCStale), so the error path here
// is the non-vacuous opposite.
func TestScanGCStaleErrorIsBestEffort(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	spy := &spyClaims{claimers: nil, gcErr: errors.New("simulated gc error")}
	r, _ := NewWithIntegrationReclaim(realReclaimer{l}, nil, nil, trunkKey, sessionKeyPrefix, spy, nil)
	rep, err := r.Scan()
	if err == nil {
		t.Fatal("a GCStale error must be surfaced (accumulated), not swallowed")
	}
	if spy.gcCalls != 1 {
		t.Fatalf("GCStale must still be invoked; got %d calls", spy.gcCalls)
	}
	if rep.GCedRecords != 0 {
		t.Fatalf("a failed GC must report zero GCed records; got %+v", rep)
	}
}

// control: with NO IntegrationClaimReclaimer wired, Scan must NOT panic and must
// leave GCedRecords == 0 (the GC hook, like the claim sweep, is gated on the
// optional reclaimer being present). Non-vacuous against TestScanInvokesGCStale.
func TestScanGCNoReclaimerLeavesZero(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l := openLedger(t, "", clk)
	r, _ := NewWithSessionKey(realReclaimer{l}, nil, nil, trunkKey, sessionKeyPrefix, nil)
	rep, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if rep.GCedRecords != 0 {
		t.Fatalf("without a reclaimer wired, no GC must run; got %+v", rep)
	}
}

// restart reattachment: a holder that died while the daemon was DOWN left an
// expired lease in the DURABLE (file-backed) ledger. A fresh Recoverer over the
// reopened ledger reclaims it on its first scan — the outcome equals live
// detection.
func TestRestartReattachment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}

	// Pre-crash: a holder acquires a lease, then the daemon (and holder) die.
	l1, err := lease.Open(path, clk)
	if err != nil {
		t.Fatal(err)
	}
	l1.Acquire(trunkKey, "dead-A", 100*time.Millisecond) // died mid-promote, holding the trunk lease
	_ = l1.Close()                                       // daemon down

	// Time passes while down; the lease expires durably.
	clk.advance(time.Hour)

	// Restart: reopen the SAME durable ledger; the first scan reattaches and
	// re-detects the mid-promote death from durable state.
	l2 := openLedger(t, path, clk)
	r, _ := New(realReclaimer{l2}, nil, nil, trunkKey, nil)
	rep, _ := r.Scan()
	if rep.Reclaimed != 1 || len(rep.DeadHolders) != 1 || rep.DeadHolders[0] != "dead-A" {
		t.Fatalf("restart reattachment must reclaim the durable expired trunk lease + detect the death; got %+v", rep)
	}
}

// no-dispatch (structural): recovery FREES resources and NEVER resumes/relaunches
// the dead agent's work — so the liveness package must never import a process
// launcher (os/exec). Asserted via the build dependency graph.
func TestLivenessNeverSpawns(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	out, err := exec.Command(goBin, "list", "-deps", "github.com/madhavhaldia/mad-substrate/internal/liveness").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v: %s", err, out)
	}
	for _, forbidden := range []string{"os/exec", "internal/launcher"} {
		if strings.Contains(string(out), forbidden) {
			t.Fatalf("no-dispatch VIOLATED: liveness imports %q (it must free resources, never spawn/resume)", forbidden)
		}
	}
}
