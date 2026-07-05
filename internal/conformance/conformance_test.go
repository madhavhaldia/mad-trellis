//go:build conformance

// conformance_test.go is the executable safety authority's own test (tag
// `conformance` — it builds a real binary and boots real daemons, so it is gated
// off the default `go test`). It:
//
//  1. builds a cgo-free mad-trellis binary to a temp dir;
//  2. runs RunGate and asserts AllPass (the self-hosting-day GREEN);
//  3. runs EVERY check's Control on a fresh hermetic Scratch and asserts each
//     control proves its check is non-vacuous (the injected violation IS caught);
//  4. the AND-not-OR META-TEST: it proves the gate is a genuine conjunction by
//     showing that when ONE conjunct's RED is surfaced, the whole gate flips RED —
//     and, conversely, that suppressing (disabling) that conjunct is the ONLY way
//     the gate stays green, i.e. no other check is silently covering for it.
//
// Run: CGO_ENABLED=1 go test -tags conformance -run . ./internal/conformance/
package conformance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles a cgo-free mad-trellis binary to a temp path the gate drives.
func buildBinary(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	out := filepath.Join(t.TempDir(), "mad-trellis-conform")
	cmd := exec.Command(goBin, "build", "-o", out, "github.com/madhavhaldia/mad-trellis/cmd/mad-trellis")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0") // cgo-free build (modernc sqlite)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mad-trellis binary: %v\n%s", err, b)
	}
	return out
}

// TestConformanceGateGreen is the acceptance gate: every registered safety check
// passes end-to-end through the public surface. This is the self-hosting-day
// signal — a green here means the safety property held.
func TestConformanceGateGreen(t *testing.T) {
	bin := buildBinary(t)
	rep, err := RunGate(bin)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	for _, r := range rep.Results {
		t.Logf("[%v] %-26s %s", boolMark(r.Pass), r.ID, r.Detail)
	}
	if !rep.AllPass {
		t.Fatalf("conformance gate RED — failed clauses: %v", rep.FailedClauses())
	}
	if len(rep.Results) == 0 {
		t.Fatal("the gate ran ZERO checks — nothing was actually asserted")
	}
}

// TestGateFailsClosedOnHarnessError proves the fail-CLOSED guarantee (fix #16):
// when the scratch cannot even BOOT (a NewScratch/daemon failure), RunGate must
// fold that harness error into a RED Result and report AllPass==false — it must
// never silently certify GREEN a clause it could not evaluate. We inject the boot
// failure by overriding the scratch factory (a clean seam — no global daemon kill).
func TestGateFailsClosedOnHarnessError(t *testing.T) {
	saved := newScratch
	t.Cleanup(func() { newScratch = saved })
	newScratch = func(string) (*Scratch, error) {
		return nil, fmt.Errorf("injected boot failure")
	}
	// A single real check; its Run never runs because the scratch boot fails first.
	checks := []Check{metaPassingCheck{}}
	rep, err := RunGateWith("ignored-binary", checks, nil)
	if err != nil {
		t.Fatalf("RunGateWith should NOT return the error (it folds it into a RED): %v", err)
	}
	if rep.AllPass {
		t.Fatal("FAIL-OPEN: the gate certified GREEN despite a harness boot failure — it must fail CLOSED")
	}
	if len(rep.Results) != 1 || rep.Results[0].Pass {
		t.Fatalf("the harness error must surface as a single RED result; got %+v", rep.Results)
	}
	if !strings.Contains(rep.Results[0].Detail, "harness error") {
		t.Fatalf("the RED result should name the harness error; got %q", rep.Results[0].Detail)
	}
}

// TestControlsAreNonVacuous runs each check's Control: every negative assertion
// must carry a positive control that genuinely detects the injected violation, so
// no "absence of behavior" assertion is vacuously green.
func TestControlsAreNonVacuous(t *testing.T) {
	bin := buildBinary(t)
	for _, c := range Checks() {
		c := c
		t.Run(c.ID(), func(t *testing.T) {
			s, err := NewScratch(bin)
			if err != nil {
				t.Fatalf("scratch: %v", err)
			}
			defer s.Close()
			if err := c.Control(s); err != nil {
				t.Fatalf("control for %q is VACUOUS: %v", c.ID(), err)
			}
		})
	}
}

// failingCheck is a synthetic conjunct that ALWAYS fails — the meta-test injects
// it to model "one conjunct's safety clause did not hold." Its presence in an
// AND-composed gate must turn the whole gate RED.
type failingCheck struct{}

func (failingCheck) ID() string           { return "meta-injected-failing-conjunct" }
func (failingCheck) OwnerProject() string { return "conformance-harness (meta-test)" }
func (failingCheck) Clause() string       { return "META: a deliberately failing conjunct" }
func (failingCheck) Run(_ *Scratch) Result {
	return Result{ID: "meta-injected-failing-conjunct", Pass: false, Detail: "injected failure"}
}
func (failingCheck) Control(_ *Scratch) error { return nil }

// TestANDNotOR_StructuralComposition is the fast STRUCTURAL AND-not-OR proof (no
// daemons): it exercises RunGateWith's composition logic directly with synthetic
// checks. It proves the gate is a genuine conjunction, not a disjunction:
//
//	(1) With one failing conjunct present ALONGSIDE a passing one, the WHOLE gate
//	    is RED. An OR-composed gate would stay green because the other passed — so
//	    a RED here is the proof of AND.
//	(2) Disabling exactly that failing conjunct restores GREEN — confirming no
//	    other check was silently covering for it.
//
// It also catches an OR-MUTATION of RunGate: if RunGateWith ORed (green if ANY
// passed) instead of ANDing, assertion (1) would fail here.
func TestANDNotOR_StructuralComposition(t *testing.T) {
	// Synthetic checks need NO daemon — override the scratch factory with a no-op so
	// the composition logic is exercised purely (a real NewScratch with a bogus
	// binary would fail-closed every check, masking the AND/OR distinction we test).
	saved := newScratch
	t.Cleanup(func() { newScratch = saved })
	newScratch = func(string) (*Scratch, error) { return &Scratch{}, nil }

	passing := metaPassingCheck{}
	failing := failingCheck{}
	checks := []Check{passing, failing}

	// (1) With the failing conjunct ENABLED, the gate must be RED even though the
	// passing one is green — this is the AND, not OR (and the OR-mutation catch).
	repRed, err := RunGateWith("ignored", checks, nil)
	if err != nil {
		t.Fatalf("RunGateWith (failing enabled): %v", err)
	}
	if repRed.AllPass {
		t.Fatal("AND-not-OR VIOLATED: the gate is GREEN with a failing conjunct present " +
			"(it is ORing checks, not ANDing them)")
	}
	if !containsID(repRed.Results, failing.ID()) || !containsID(repRed.Results, passing.ID()) {
		t.Fatal("both conjuncts must be evaluated (the meta-test would otherwise be hollow)")
	}
	// The passing conjunct's own Result must still be PASS — a RED gate must not
	// smear failure onto an innocent conjunct.
	if !resultPass(repRed.Results, passing.ID()) {
		t.Fatal("the passing conjunct's Result was not PASS in a RED gate (failure must not mask another conjunct)")
	}

	// (2) Disabling exactly the failing conjunct restores GREEN.
	repGreen, err := RunGateWith("ignored", checks, map[string]bool{failing.ID(): true})
	if err != nil {
		t.Fatalf("RunGateWith (failing disabled): %v", err)
	}
	if !repGreen.AllPass {
		t.Fatalf("disabling the failing conjunct should restore GREEN; got RED: %v", repGreen.FailedClauses())
	}
	if containsID(repGreen.Results, failing.ID()) {
		t.Fatal("the disabled conjunct should have been SKIPPED, but it was evaluated")
	}
}

// TestEveryConjunctGatesTheGate is the REAL per-conjunct AND-not-OR proof (fix #6).
// The prior version composed a gate of EXACTLY ONE element ([]Check{forceFail{c}}),
// which is RED even under an OR-composed gate — so it did NOT prove AND. THIS
// version composes the FULL registered set with ONLY the target conjunct wrapped in
// forceFail, and asserts:
//
//	(a) the whole gate is RED (AllPass==false) — even though every OTHER conjunct
//	    is its real, passing self. Under an OR gate this would be GREEN (the others
//	    pass), so the RED is the genuine AND proof; AND
//	(b) every OTHER conjunct's Result.Pass==true — proving no conjunct masks
//	    another, and that the RED is attributable solely to the forced-failed one.
//
// This is run for EVERY registered check, so no conjunct is dead weight in the
// conjunction and each is independently load-bearing.
func TestEveryConjunctGatesTheGate(t *testing.T) {
	bin := buildBinary(t)
	all := Checks()
	if len(all) == 0 {
		t.Fatal("no checks registered — nothing to prove")
	}

	for _, target := range all {
		target := target
		t.Run(target.ID(), func(t *testing.T) {
			// The FULL set, with ONLY this conjunct forced to fail; everything else is
			// its real passing self.
			composed := make([]Check, 0, len(all))
			for _, c := range all {
				if c.ID() == target.ID() {
					composed = append(composed, forceFail{c})
				} else {
					composed = append(composed, c)
				}
			}
			rep, err := RunGateWith(bin, composed, nil)
			if err != nil {
				t.Fatalf("RunGateWith (forced-fail %q in full set): %v", target.ID(), err)
			}
			// (a) the gate is RED despite every other conjunct passing → genuine AND.
			if rep.AllPass {
				t.Fatalf("AND-not-OR VIOLATED for %q: forcing ONLY this conjunct to FAIL (with the full "+
					"passing set alongside) left the gate GREEN — it is ORing, or this conjunct does not gate", target.ID())
			}
			if !containsID(rep.Results, target.ID()) {
				t.Fatalf("the forced-fail conjunct %q was not even evaluated", target.ID())
			}
			// The forced-fail conjunct must be the failing one.
			if resultPass(rep.Results, target.ID()) {
				t.Fatalf("the forced-fail conjunct %q reported PASS — forceFail did not take effect", target.ID())
			}
			// (b) every OTHER conjunct passed — no conjunct masks another, and the RED
			// is solely attributable to the forced failure.
			for _, c := range all {
				if c.ID() == target.ID() {
					continue
				}
				if !resultPass(rep.Results, c.ID()) {
					t.Errorf("conjunct %q FAILED when only %q was forced to fail — a conjunct is masking "+
						"another (or is flaky); each conjunct must pass independently", c.ID(), target.ID())
				}
			}
		})
	}
}

// TestCoverageMatrixComplete asserts the coverage matrix is COMPLETE: every
// registered check maps to a non-empty 0003 clause string AND an owning project
// id, and every check id is unique. An opaque pass/fail without a clause+owner
// would make the gate un-auditable against the docs/0003 clause map.
func TestCoverageMatrixComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Checks() {
		if c.ID() == "" {
			t.Error("a registered check has an empty ID")
		}
		if seen[c.ID()] {
			t.Errorf("duplicate check id %q in the coverage matrix", c.ID())
		}
		seen[c.ID()] = true
		if strings.TrimSpace(c.Clause()) == "" {
			t.Errorf("check %q has an empty Clause() — the coverage matrix would not map it to a 0003 clause", c.ID())
		}
		if strings.TrimSpace(c.OwnerProject()) == "" {
			t.Errorf("check %q has an empty OwnerProject() — the coverage matrix would not map it to an owning project", c.ID())
		}
	}
	if len(seen) == 0 {
		t.Fatal("no checks registered — the coverage matrix is empty")
	}
}

// metaPassingCheck is a synthetic always-pass conjunct (no daemon needed) used by
// the AND-not-OR meta-test as the "other checks pass" backdrop.
type metaPassingCheck struct{}

func (metaPassingCheck) ID() string           { return "meta-passing-conjunct" }
func (metaPassingCheck) OwnerProject() string { return "conformance-harness (meta-test)" }
func (metaPassingCheck) Clause() string       { return "META: a deliberately passing conjunct" }
func (metaPassingCheck) Run(_ *Scratch) Result {
	return Result{ID: "meta-passing-conjunct", Pass: true, Detail: "ok"}
}
func (metaPassingCheck) Control(_ *Scratch) error { return nil }

// forceFail wraps a real Check so its Run always FAILS, preserving its identity —
// the meta-test's "disable/break this conjunct and assert RED" applied to a real
// conjunct.
type forceFail struct{ Check }

func (f forceFail) Run(s *Scratch) Result {
	r := f.Check.Run(s) // run the real probe (it should pass), then force RED
	r.Pass = false
	r.Detail = "META forced failure of a real conjunct (was: " + r.Detail + ")"
	return r
}

func containsID(rs []Result, id string) bool {
	for _, r := range rs {
		if r.ID == id {
			return true
		}
	}
	return false
}

// resultPass reports whether the Result for id is present AND passed. A missing id
// returns false (an absent conjunct cannot be said to have passed).
func resultPass(rs []Result, id string) bool {
	for _, r := range rs {
		if r.ID == id {
			return r.Pass
		}
	}
	return false
}

func boolMark(b bool) string {
	if b {
		return "PASS"
	}
	return "FAIL"
}
