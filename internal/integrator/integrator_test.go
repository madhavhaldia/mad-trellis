package integrator

// Hand-authored invariant suite for project 6 (integrator-trunk) — the contract,
// authored by hand (REVIEW-GATED). Each owned clause maps to a proving test, and
// every absence-assertion carries a positive control proving it is non-vacuous:
//
//   5-integrator (one promoter)      → TestSingleIntegratorExclusion (+real-ledger)
//   6 validated-gate (no silent merge)→ TestValidationGateMandatory (+control)
//   6 single-writer is the lease      → TestLeaseGatedPromotion (+control)
//   6/7 by construction (atomic)      → TestPromoteCrashConsistency (kill each boundary)
//   idempotency                       → TestIdempotentRePromote / TestIdempotentAbort
//   2(b) no probabilistic component   → TestPromoteDecisionDeterministic (+control)
//   liveness one-way (never import)   → TestIntegratorDoesNotImportLiveness
// (origin-bypass + mediated remote are in mediated_test.go.)

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fixture -----------------------------------------------------------------

// fix is a bare mediated trunk repo (with the protective hook) plus an author
// working repo whose origin is that bare repo, so feature branches are pushed as
// agents would and the integrator promotes them onto trunk.
type fix struct {
	t      *testing.T
	bare   string // the integrator trunk/holding repo (bare, hook installed)
	branch string // "trunk"
	work   string // an author working repo, origin -> bare
	base   string // the established trunk base commit (in bare)
}

func newFix(t *testing.T) *fix {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "trunk.git")
	if _, err := EnsureMediatedRepo(bare, "trunk"); err != nil {
		t.Fatalf("ensure mediated: %v", err)
	}
	work := t.TempDir()
	mustGit(t, "", "init", "-q", "-b", "main", work)
	mustGit(t, work, "config", "user.email", "t@t")
	mustGit(t, work, "config", "user.name", "t")
	mustGit(t, work, "remote", "add", "origin", bare)
	f := &fix{t: t, bare: bare, branch: "trunk", work: work}

	// Establish trunk with a base commit: commit a.txt in work, push it as an
	// agent branch (nm/* is allowed), then point trunk at it via a LOCAL
	// update-ref in the bare repo (legit setup — the same plumbing the integrator
	// uses; a push to trunk is rejected by the hook, which TestMediatedRepo proves).
	write(t, work, "a.txt", "base\n")
	mustGit(t, work, "add", ".")
	mustGit(t, work, "commit", "-q", "-m", "base")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/nm/base")
	f.base = revParse(t, work, "HEAD")
	mustGit(t, bare, "update-ref", "refs/heads/trunk", f.base)
	return f
}

// pushBranch creates a feature branch off `fromRef` in the work repo, applies
// files, commits, and pushes it to the bare repo as refs/heads/nm/<name>. It
// returns the bare-side ref name the integrator submits.
func (f *fix) pushBranch(name, fromRef string, files map[string]string) string {
	t := f.t
	t.Helper()
	mustGit(t, f.work, "fetch", "-q", "origin")
	wb := "wb-" + name
	// Branch off the requested ref (origin/trunk or origin/nm/...).
	mustGit(t, f.work, "checkout", "-q", "-B", wb, fromRef)
	for path, content := range files {
		write(t, f.work, path, content)
	}
	mustGit(t, f.work, "add", "-A")
	mustGit(t, f.work, "commit", "-q", "-m", "feat "+name)
	mustGit(t, f.work, "push", "-q", "origin", "HEAD:refs/heads/nm/"+name)
	return "refs/heads/nm/" + name
}

func (f *fix) newIntegrator(lg LeaseGate) *Integrator {
	t := f.t
	t.Helper()
	if lg == nil {
		lg = newFakeLease()
	}
	it, err := New(Options{
		TrunkDir:    f.bare,
		TrunkBranch: f.branch,
		StorePath:   filepath.Join(t.TempDir(), "i.db"),
		Leases:      lg,
		TrunkKey:    []byte("mad-substrate:trunk:v1"),
	})
	if err != nil {
		t.Fatalf("new integrator: %v", err)
	}
	t.Cleanup(func() { _ = it.Close() })
	return it
}

func (f *fix) trunkTip() string { return revParse(f.t, f.bare, "refs/heads/trunk") }

// --- fake lease gate ---------------------------------------------------------

type fakeLease struct {
	mu          sync.Mutex
	held        map[string]string
	failAcquire bool   // simulate "trunk held by someone else"
	forcedHold  string // a non-empty holder reported when failAcquire
	acquires    int
}

func newFakeLease() *fakeLease { return &fakeLease{held: map[string]string{}, forcedHold: "other"} }

func (f *fakeLease) Acquire(key []byte, holder string, _ time.Duration) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquires++
	if f.failAcquire {
		return false, f.forcedHold, nil
	}
	k := string(key)
	if h, ok := f.held[k]; ok && h != holder {
		return false, h, nil
	}
	f.held[k] = holder
	return true, holder, nil
}

func (f *fakeLease) Release(key []byte, holder string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := string(key)
	if f.held[k] == holder {
		delete(f.held, k)
		return true, nil
	}
	return false, nil
}

// --- tests -------------------------------------------------------------------

// [6] a clean branch promotes: the gate fires, trunk advances atomically, and
// the integration is promoted exactly once.
func TestPromoteCleanAdvancesTrunk(t *testing.T) {
	f := newFix(t)
	it := f.newIntegrator(nil)
	br := f.pushBranch("clean", "origin/trunk", map[string]string{"b.txt": "added\n"})

	rec, err := it.Submit("s-1", br)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	before := f.trunkTip()
	out, err := it.Promote("s-1", rec.ID)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !out.Promoted || out.State != StatePromoted {
		t.Fatalf("want promoted; got %+v", out)
	}
	after := f.trunkTip()
	if after == before {
		t.Fatal("trunk did not advance on a clean promote")
	}
	if after != out.TrunkTip {
		t.Fatalf("trunk tip %s != reported %s", after, out.TrunkTip)
	}
	// The promoted commit must contain BOTH the base file and the branch's file.
	if !commitHasFile(t, f.bare, after, "a.txt") || !commitHasFile(t, f.bare, after, "b.txt") {
		t.Fatal("promoted tree must merge base + branch content")
	}
}

// [6] VALIDATION-GATE-MANDATORY: a conflicting branch is REJECTED — it never
// reaches promoted and the trunk is untouched (nothing merges silently). The
// positive control (the clean branch above) proves the gate is not vacuously
// rejecting everything.
func TestValidationGateMandatory(t *testing.T) {
	f := newFix(t)
	it := f.newIntegrator(nil)

	// Advance trunk so a.txt diverges from a stale branch.
	clean := f.pushBranch("trunkchange", "origin/trunk", map[string]string{"a.txt": "trunk-version\n"})
	r1, _ := it.Submit("s-1", clean)
	if _, err := it.Promote("s-1", r1.ID); err != nil {
		t.Fatalf("setup promote: %v", err)
	}
	trunkAfterSetup := f.trunkTip()

	// A branch off the ORIGINAL base that edits the same line → conflicts with trunk.
	conflict := f.pushBranch("conflict", "origin/nm/base", map[string]string{"a.txt": "conflicting\n"})
	r2, _ := it.Submit("s-2", conflict)
	out, err := it.Promote("s-2", r2.ID)
	if err != nil {
		t.Fatalf("promote(conflict): unexpected error %v", err)
	}
	if out.Promoted || out.State != StateAborted {
		t.Fatalf("a conflicting branch must NOT promote; got %+v", out)
	}
	if f.trunkTip() != trunkAfterSetup {
		t.Fatal("a rejected integration must leave trunk byte-identical")
	}
}

// [6 single-writer] LEASE-GATED PROMOTION: with the trunk lease unavailable the
// promote refuses (no promote without the lease) and trunk is untouched.
// +control: the same integration, once the lease is available, DOES promote —
// proving the refusal was the lease, not an unrelated failure.
func TestLeaseGatedPromotion(t *testing.T) {
	f := newFix(t)
	lg := newFakeLease()
	it := f.newIntegrator(lg)
	br := f.pushBranch("x", "origin/trunk", map[string]string{"b.txt": "x\n"})
	rec, _ := it.Submit("s-1", br)
	before := f.trunkTip()

	lg.failAcquire = true
	out, err := it.Promote("s-1", rec.ID)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if out.Promoted {
		t.Fatal("FAIL-OPEN: promoted without the trunk lease")
	}
	if !out.Retryable || !strings.Contains(out.Reason, "busy") {
		t.Fatalf("lease-denied promote must be retryable+busy; got %+v", out)
	}
	if f.trunkTip() != before {
		t.Fatal("a lease-denied promote must not advance trunk")
	}

	// +control: lease now available → promotes (the gate above was the lease).
	lg.failAcquire = false
	out2, err := it.Promote("s-1", rec.ID)
	if err != nil {
		t.Fatalf("promote(retry): %v", err)
	}
	if !out2.Promoted {
		t.Fatalf("once the lease is available the same integration must promote; got %+v", out2)
	}
	if f.trunkTip() == before {
		t.Fatal("control: trunk must advance once the lease is granted")
	}
}

// [5-integrator] SINGLE PROMOTER, against the REAL ledger CAS: while one session
// holds the trunk lease, a second concurrent promote fails fast (CAS-fail-fast),
// never a second writer. Uses a real-ledger adapter to prove it is genuinely the
// lease, not a private mutex.
func TestSingleIntegratorExclusion(t *testing.T) {
	f := newFix(t)
	rl := newRealLeaseGate(t)
	it := f.newIntegrator(rl)
	key := []byte("mad-substrate:trunk:v1")

	// Hold the trunk lease under a DIFFERENT session, as a rival integrator would.
	granted, _, err := rl.Acquire(key, "rival", 60*time.Second)
	if err != nil || !granted {
		t.Fatalf("setup acquire: granted=%v err=%v", granted, err)
	}
	br := f.pushBranch("x", "origin/trunk", map[string]string{"b.txt": "x\n"})
	rec, _ := it.Submit("s-1", br)
	before := f.trunkTip()
	out, err := it.Promote("s-1", rec.ID)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if out.Promoted {
		t.Fatal("two promoters at once: the second must NOT advance trunk")
	}
	if !out.Retryable {
		t.Fatalf("a lease-contended promote must be retryable; got %+v", out)
	}
	if f.trunkTip() != before {
		t.Fatal("the rival-held promote must leave trunk untouched")
	}
	// Release the rival's hold → the integration now promotes.
	if _, err := rl.Release(key, "rival"); err != nil {
		t.Fatal(err)
	}
	out2, _ := it.Promote("s-1", rec.ID)
	if !out2.Promoted {
		t.Fatalf("after the rival releases, the promote must succeed; got %+v", out2)
	}
}

// [6/7 BY CONSTRUCTION] crash-consistency: inject a death at EACH state boundary
// and assert trunk is never half-applied — byte-identical before the atomic
// update-ref, and equal to the merge commit after it (atomic). Abort/recovery
// read git to reconcile.
func TestPromoteCrashConsistency(t *testing.T) {
	cases := []struct {
		point           string
		trunkAdvances   bool // is trunk == merge_commit after the fault?
		abortReconciles State
	}{
		{"after-validating", false, StateAborted},
		{"after-lease", false, StateAborted},
		{"before-updateref", false, StateAborted},
		{"after-updateref", true, StatePromoted}, // atomic advance landed; abort is a terminal no-op
	}
	for _, tc := range cases {
		t.Run(tc.point, func(t *testing.T) {
			f := newFix(t)
			it := f.newIntegrator(nil)
			br := f.pushBranch("c", "origin/trunk", map[string]string{"b.txt": "c\n"})
			rec, _ := it.Submit("s-1", br)
			base := f.trunkTip()

			it.fault = func(p string) error {
				if p == tc.point {
					return errSimulatedDeath
				}
				return nil
			}
			_, err := it.Promote("s-1", rec.ID)
			if err != errSimulatedDeath {
				t.Fatalf("expected simulated death at %s; got %v", tc.point, err)
			}

			tip := f.trunkTip()
			if tc.trunkAdvances {
				if tip == base {
					t.Fatal("after-updateref: the atomic advance must have landed")
				}
			} else {
				if tip != base {
					t.Fatalf("death at %s left trunk HALF-APPLIED: %s != base %s", tc.point, tip, base)
				}
			}

			// Liveness invokes Abort on the dead holder; clear the fault first
			// (the "process" that died is gone; Abort runs in the survivor).
			it.fault = nil
			out, err := it.Abort(rec.ID)
			if err != nil {
				t.Fatalf("abort: %v", err)
			}
			if out.State != tc.abortReconciles {
				t.Fatalf("abort reconcile: want %s got %s", tc.abortReconciles, out.State)
			}
			// Trunk is clean either way: base (pre-advance) or merge_commit (post).
			final := f.trunkTip()
			if tc.trunkAdvances {
				if final != tip {
					t.Fatal("post-advance abort must not move trunk")
				}
			} else if final != base {
				t.Fatalf("abort after a pre-advance death must leave trunk at base; got %s", final)
			}
		})
	}
}

// [idempotency] re-promoting a promoted integration is a no-op that returns
// promoted; the trunk does not move a second time.
func TestIdempotentRePromote(t *testing.T) {
	f := newFix(t)
	it := f.newIntegrator(nil)
	br := f.pushBranch("x", "origin/trunk", map[string]string{"b.txt": "x\n"})
	rec, _ := it.Submit("s-1", br)
	out1, _ := it.Promote("s-1", rec.ID)
	if !out1.Promoted {
		t.Fatalf("first promote must succeed; got %+v", out1)
	}
	tip1 := f.trunkTip()
	out2, err := it.Promote("s-1", rec.ID)
	if err != nil {
		t.Fatalf("re-promote: %v", err)
	}
	if !out2.Promoted || f.trunkTip() != tip1 {
		t.Fatalf("re-promote must be a no-op returning promoted; got %+v tip=%s", out2, f.trunkTip())
	}
}

// [idempotency] abort is idempotent across states: clean (received), mid
// (validating via fault), already-aborted, and terminal (promoted → stays
// promoted, never reverts trunk).
func TestIdempotentAbort(t *testing.T) {
	t.Run("received", func(t *testing.T) {
		f := newFix(t)
		it := f.newIntegrator(nil)
		br := f.pushBranch("x", "origin/trunk", map[string]string{"b.txt": "x\n"})
		rec, _ := it.Submit("s-1", br)
		base := f.trunkTip()
		o1, _ := it.Abort(rec.ID)
		o2, _ := it.Abort(rec.ID)
		if o1.State != StateAborted || o2.State != StateAborted {
			t.Fatalf("double abort must stay aborted; got %s,%s", o1.State, o2.State)
		}
		if f.trunkTip() != base {
			t.Fatal("abort must not touch trunk")
		}
		// A promote of an aborted id errors (cannot resurrect).
		if _, err := it.Promote("s-1", rec.ID); err != ErrAborted {
			t.Fatalf("promote of aborted must error ErrAborted; got %v", err)
		}
	})
	t.Run("already-promoted", func(t *testing.T) {
		f := newFix(t)
		it := f.newIntegrator(nil)
		br := f.pushBranch("x", "origin/trunk", map[string]string{"b.txt": "x\n"})
		rec, _ := it.Submit("s-1", br)
		it.Promote("s-1", rec.ID)
		tip := f.trunkTip()
		out, _ := it.Abort(rec.ID)
		if out.State != StatePromoted || f.trunkTip() != tip {
			t.Fatalf("abort of a promoted integration must be a terminal no-op; got %+v", out)
		}
	})
	t.Run("absent", func(t *testing.T) {
		f := newFix(t)
		it := f.newIntegrator(nil)
		out, err := it.Abort("int-does-not-exist")
		if err != nil {
			t.Fatalf("abort of an unknown id must be a safe no-op; got %v", err)
		}
		if out.State != "" {
			t.Fatalf("abort of an unknown id should report no state; got %q", out.State)
		}
	})
}

// [2b] the promote decision is a deterministic function of the repo contents:
// the same (trunk, branch) yields the same gate verdict every time. +control: a
// deliberately nondeterministic gate is DETECTED as nondeterministic, proving
// the determinism assertion is non-vacuous (a probabilistic/LLM gate would fail
// it). The cross-project static no-LLM reachability is owned by 10a.
func TestPromoteDecisionDeterministic(t *testing.T) {
	f := newFix(t)
	tr := newTrunkRepo(f.bare, "trunk")
	br := f.pushBranch("x", "origin/trunk", map[string]string{"b.txt": "x\n"})
	trunkOID := f.trunkTip()
	branchOID := revParse(t, f.bare, br)

	g := mergeGate{}
	first, err := g.Validate(tr, trunkOID, branchOID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		r, err := g.Validate(tr, trunkOID, branchOID)
		if err != nil {
			t.Fatal(err)
		}
		if r != first {
			t.Fatalf("mergeGate is nondeterministic: %+v != %+v", r, first)
		}
	}

	// +control: a coin-flip gate IS nondeterministic — the assertion above would
	// fail for it, so it is non-vacuous.
	flip := &flipGate{}
	a, _ := flip.Validate(tr, trunkOID, branchOID)
	b, _ := flip.Validate(tr, trunkOID, branchOID)
	if a == b {
		t.Fatal("control gate should differ across calls (non-vacuity check)")
	}
}

type flipGate struct{ n int }

func (g *flipGate) Validate(_ *trunkRepo, _, _ string) (GateResult, error) {
	g.n++
	return GateResult{OK: g.n%2 == 0, Reason: "flip"}, nil
}

// [liveness one-way] the integrator package must never import liveness-recovery
// (the dependency is one-way: liveness depends on the integrator). Asserted via
// the build dependency graph so it survives once liveness exists.
func TestIntegratorDoesNotImportLiveness(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	out, err := exec.Command(goBin, "list", "-deps", "github.com/madhavhaldia/mad-substrate/internal/integrator").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v: %s", err, out)
	}
	if strings.Contains(string(out), "internal/liveness") {
		t.Fatal("Inv build-order VIOLATED: integrator imports liveness (must be one-way)")
	}
}

// [6/7 by construction — the SUPERSEDED case] Regression for the exact-tip-
// equality reconcile bug: X advances trunk then dies after-updateref (row stays
// validating); a LATER integration Y promotes ON TOP, moving trunk PAST X's merge
// commit. Recovery must still recognise X as PROMOTED (its merge commit is a
// permanent ancestor of trunk) — Abort/Status/re-Promote reconcile by ANCESTRY,
// not exact-tip equality, and a re-Promote must NOT double-apply X's work.
func TestPromoteCrashConsistencySuperseded(t *testing.T) {
	f := newFix(t)
	it := f.newIntegrator(nil)

	brX := f.pushBranch("x", "origin/trunk", map[string]string{"x.txt": "x\n"})
	recX, _ := it.Submit("s-1", brX)
	it.fault = func(p string) error {
		if p == "after-updateref" {
			return errSimulatedDeath
		}
		return nil
	}
	if _, err := it.Promote("s-1", recX.ID); err != errSimulatedDeath {
		t.Fatalf("want simulated death after-updateref; got %v", err)
	}
	it.fault = nil
	tipX := f.trunkTip() // trunk == X's merge commit; X's row is still `validating`

	// Y promotes on top → trunk advances PAST X's merge commit.
	brY := f.pushBranch("y", "origin/trunk", map[string]string{"y.txt": "y\n"})
	recY, _ := it.Submit("s-2", brY)
	outY, err := it.Promote("s-2", recY.ID)
	if err != nil || !outY.Promoted {
		t.Fatalf("Y must promote on top of X; got %+v %v", outY, err)
	}
	tipY := f.trunkTip()
	if tipY == tipX {
		t.Fatal("Y must have advanced trunk past X")
	}

	// Status(X): X is landed-then-superseded → must report PROMOTED, not validating.
	st, err := it.Status(recX.ID)
	if err != nil || st.State != StatePromoted || !st.Promoted {
		t.Fatalf("superseded X must reconcile to PROMOTED; got %+v %v", st, err)
	}
	// Abort(X): must be a terminal no-op reporting promoted (never records aborted
	// for a landed integration), and must NOT move trunk.
	ab, err := it.Abort(recX.ID)
	if err != nil || ab.State != StatePromoted {
		t.Fatalf("abort of a superseded-but-landed X must report PROMOTED; got %+v %v", ab, err)
	}
	if f.trunkTip() != tipY {
		t.Fatal("abort of a landed integration must not move trunk")
	}
	// re-Promote(X): idempotent no-op; must NOT create a second (duplicate) advance.
	rp, err := it.Promote("s-1", recX.ID)
	if err != nil || !rp.Promoted {
		t.Fatalf("re-promote of superseded X must report promoted; got %+v %v", rp, err)
	}
	if f.trunkTip() != tipY {
		t.Fatal("re-promote of a landed integration must NOT double-advance trunk")
	}
}

// [statemachine] commitMerge is a DETERMINISTIC function of (tree, parents,
// message, date): two calls with identical inputs yield the identical commit OID
// (so a concurrent same-id Promote cannot diverge the durable merge_commit from
// the promoted trunk ref). +control: a different message yields a different OID.
func TestCommitMergeDeterministic(t *testing.T) {
	f := newFix(t)
	tr := newTrunkRepo(f.bare, "trunk")
	br := f.pushBranch("x", "origin/trunk", map[string]string{"b.txt": "x\n"})
	trunkOID := f.trunkTip()
	branchOID := revParse(t, f.bare, br)
	tree, clean, _, err := tr.mergeTree(trunkOID, branchOID)
	if err != nil || !clean {
		t.Fatalf("merge-tree: clean=%v err=%v", clean, err)
	}
	date := time.Unix(1700000000, 0)
	c1, err := tr.commitMerge(tree, trunkOID, branchOID, "msg id=abc", date)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := tr.commitMerge(tree, trunkOID, branchOID, "msg id=abc", date)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatalf("commitMerge must be deterministic per (tree,parents,msg,date); %s != %s", c1, c2)
	}
	c3, err := tr.commitMerge(tree, trunkOID, branchOID, "msg id=different", date)
	if err != nil {
		t.Fatal(err)
	}
	if c3 == c1 {
		t.Fatal("control: a different message must yield a different commit OID")
	}
}

// [Inv 4 / availability] RPC-abort is HOLDER-BOUND: only the integration's own
// holder may cancel it via AbortAs; a non-holder is refused (ErrUnauthorized)
// WITHOUT changing state. +control: the holder CAN abort, and the privileged
// in-process Abort(id) (the liveness path) works regardless of session.
func TestAbortAsHolderBound(t *testing.T) {
	f := newFix(t)
	it := f.newIntegrator(nil)
	br := f.pushBranch("x", "origin/trunk", map[string]string{"b.txt": "x\n"})
	rec, _ := it.Submit("holder-A", br)

	if _, err := it.AbortAs("attacker-B", rec.ID); err != ErrUnauthorized {
		t.Fatalf("a non-holder abort must be ErrUnauthorized; got %v", err)
	}
	if st, _ := it.Status(rec.ID); st.State != StateReceived {
		t.Fatalf("a refused abort must not change state; got %s", st.State)
	}
	out, err := it.AbortAs("holder-A", rec.ID)
	if err != nil || out.State != StateAborted {
		t.Fatalf("the holder must be able to abort; got %+v %v", out, err)
	}
	// The privileged in-process Abort (liveness, dead holder whose session is gone)
	// is NOT holder-bound.
	rec2, _ := it.Submit("holder-C", br)
	o2, err := it.Abort(rec2.ID)
	if err != nil || o2.State != StateAborted {
		t.Fatalf("privileged in-process Abort must work; got %+v %v", o2, err)
	}
}

// [gate] isHexOID discriminates a merge-tree CONFLICT (merged tree OID printed
// first) from a git USAGE error (no leading OID), so a real error is surfaced as
// an error, never misreported as a conflict-reject.
func TestIsHexOID(t *testing.T) {
	for _, ok := range []string{strings.Repeat("a", 40), strings.Repeat("0", 64), "0123456789abcdef0123456789abcdef01234567"} {
		if !isHexOID(ok) {
			t.Fatalf("%q must be recognized as an OID", ok)
		}
	}
	for _, bad := range []string{"", "xyz", strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("g", 40), "fatal: not something we can merge"} {
		if isHexOID(bad) {
			t.Fatalf("%q must NOT be recognized as an OID", bad)
		}
	}
}

// Submit must reject a client-controlled branch ref that could be parsed as a
// git option / expand into an unintended revision (arg injection). +control: a
// legitimate nm/* ref is accepted (above tests), so the rejection is not vacuous.
func TestSubmitRejectsUnsafeRef(t *testing.T) {
	f := newFix(t)
	it := f.newIntegrator(nil)
	for _, bad := range []string{
		"--upload-pack=/bin/sh",
		"-x",
		"refs/heads/../../etc/passwd",
		"HEAD@{0}",
		"trunk^{tree}",
		"a b",
		"refs/heads/nm/x:refs/heads/trunk",
	} {
		if _, err := it.Submit("s-1", bad); err == nil {
			t.Fatalf("unsafe ref %q must be rejected", bad)
		}
	}
}

// --- helpers -----------------------------------------------------------------

var errSimulatedDeath = &simErr{}

type simErr struct{}

func (*simErr) Error() string { return "simulated death" }

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var full []string
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, args...)
	c := exec.Command("git", full...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func revParse(t *testing.T, dir, rev string) string {
	t.Helper()
	return mustGit(t, dir, "rev-parse", "--verify", rev+"^{commit}")
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitHasFile(t *testing.T, repo, commit, path string) bool {
	t.Helper()
	c := exec.Command("git", "-C", repo, "cat-file", "-e", commit+":"+path)
	return c.Run() == nil
}
