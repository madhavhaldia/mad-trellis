package conformance

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// gate.go composes ALL registered checks into ONE authoritative verdict. RunGate
// is AND-not-OR: the gate is GREEN iff EVERY check passed (it fails if ANY single
// check fails). This is the self-hosting-day signal — one green = the safety
// property held end-to-end through the public surface; one red = it did not.
//
// The AND-not-OR discipline is load-bearing (docs/0004 §10a "the short-circuit/OR
// bug"): a gate that ORed its checks (green if ANY passed) would mask a real
// safety failure. conformance_test.go's META-TEST disables one conjunct and
// asserts the gate flips RED — proving the composition is a genuine conjunction.

// Report is the composed gate verdict.
type Report struct {
	Results []Result // one per registered check, in registration order
	AllPass bool     // AND of every check (the self-hosting-day signal)
}

// CoverageRow maps one check's assertion to its 0003 clause + owning project — the
// COVERAGE MATRIX printed by `mad-substrate conform`, so the gate is auditable against
// the clause map (docs/0003 L20-39) rather than an opaque pass/fail.
type CoverageRow struct {
	CheckID      string
	Clause       string
	OwnerProject string
	Pass         bool
	Skipped      bool // the check could not be evaluated in this environment (rendered [SKIP])
}

// RunGate runs the production gate: every REGISTERED check, nothing disabled. It
// is a thin wrapper over RunGateWith so production callers never touch a global
// switch. binaryPath is the mad-substrate binary the checks drive (the conform CLI
// passes os.Executable(); the tagged test builds one to temp).
func RunGate(binaryPath string) (Report, error) {
	return RunGateWith(binaryPath, Checks(), nil)
}

// RunGateWith runs an EXPLICIT set of checks against a FRESH hermetic Scratch per
// check (so no check's residue — a held lease, a manifest, an advanced trunk —
// perturbs another) and ANDs the verdicts. A check whose ID is in `disabled` is
// SKIPPED (the meta-test hook). Passing the checks + disabled set as PARAMETERS
// (rather than mutating package globals) is what lets the AND-not-OR meta-tests
// run in parallel without a t.Cleanup global swap and a latent data race
// (fix #17): every caller composes its OWN gate explicitly.
//
// AND-not-OR is load-bearing (docs/0004 §10a "the short-circuit/OR bug"): the
// report is GREEN iff EVERY non-disabled check passed; a SINGLE failure (or a
// harness error, which is itself a RED — fix #16) makes the whole gate RED. A
// gate that ORed would stay green while a clause silently failed.
func RunGateWith(binaryPath string, checks []Check, disabled map[string]bool) (Report, error) {
	var rep Report
	rep.AllPass = true
	for _, c := range checks {
		if disabled[c.ID()] {
			continue // conjunct turned off (meta-test only)
		}
		r, err := runOne(binaryPath, c)
		if err != nil {
			// A harness failure (could not boot the scratch daemon, a NewScratch/boot
			// error) is itself a RED — the gate cannot certify safety it could not
			// evaluate. This is the fail-CLOSED guarantee on harness errors (fix #16):
			// RunGate never returns the error to a caller who might ignore it; it folds
			// the failure into a RED Result so AllPass is false.
			r = Result{ID: c.ID(), Clause: c.Clause(), OwnerProject: c.OwnerProject(), Pass: false,
				Detail: "harness error: " + err.Error()}
		}
		rep.Results = append(rep.Results, r)
		// AND-not-OR: a single failure makes the whole gate RED.
		if !r.Pass {
			rep.AllPass = false
		}
	}
	return rep, nil
}

// newScratch is the scratch factory runOne uses. It defaults to NewScratch; a
// test overrides it to inject a boot/NewScratch failure and assert the gate folds
// that harness error into a RED (fail-closed, fix #16).
var newScratch = NewScratch

// runOne boots a fresh Scratch and runs a single check's Run, guaranteeing the
// scratch is torn down even if the check panics.
func runOne(binaryPath string, c Check) (res Result, err error) {
	s, serr := newScratch(binaryPath)
	if serr != nil {
		return Result{}, serr
	}
	defer s.Close()
	defer func() {
		if r := recover(); r != nil {
			res = Result{ID: c.ID(), Clause: c.Clause(), OwnerProject: c.OwnerProject(), Pass: false,
				Detail: fmt.Sprintf("check panicked: %v", r)}
			err = nil
		}
	}()
	return c.Run(s), nil
}

// Coverage projects a Report to its coverage matrix (assertion -> clause + owner).
func (rep Report) Coverage() []CoverageRow {
	rows := make([]CoverageRow, 0, len(rep.Results))
	for _, r := range rep.Results {
		rows = append(rows, CoverageRow{CheckID: r.ID, Clause: r.Clause, OwnerProject: r.OwnerProject, Pass: r.Pass, Skipped: r.Skipped})
	}
	return rows
}

// Print writes the coverage matrix, per-check PASS/FAIL with detail, and the final
// GREEN/RED to w (the format `mad-substrate conform` renders). Deterministic ordering:
// registration order (the order safety conjuncts are argued).
func (rep Report) Print(w io.Writer) {
	fmt.Fprintln(w, "mad-substrate conformance — safety-property authority (self-hosting-day gate)")
	fmt.Fprintln(w, strings.Repeat("=", 78))
	fmt.Fprintln(w, "COVERAGE MATRIX (assertion -> 0003 clause / owning project):")
	for _, row := range rep.Coverage() {
		mark := coverageMark(row.Pass, row.Skipped)
		fmt.Fprintf(w, "  [%s] %-26s  %s\n", mark, row.CheckID, row.Clause)
		fmt.Fprintf(w, "         %sowner: %s\n", strings.Repeat(" ", 26), row.OwnerProject)
	}
	fmt.Fprintln(w, strings.Repeat("-", 78))
	for _, r := range rep.Results {
		mark := coverageMark(r.Pass, r.Skipped)
		fmt.Fprintf(w, "  [%s] %-26s  %s\n", mark, r.ID, r.Detail)
	}
	fmt.Fprintln(w, strings.Repeat("=", 78))
	if rep.AllPass {
		fmt.Fprintln(w, "RESULT: GREEN — the safety property held end-to-end (self-hosting day).")
	} else {
		fmt.Fprintln(w, "RESULT: RED — a safety clause FAILED. Not safe to self-host.")
	}
}

// coverageMark renders one check's mark: SKIP (could not be evaluated — e.g. a
// runtime-less host), PASS, or FAIL. SKIP is rendered DISTINCTLY so an
// un-evaluated structural assertion is never a silent green.
func coverageMark(pass, skipped bool) string {
	switch {
	case skipped:
		return "SKIP"
	case pass:
		return "PASS"
	default:
		return "FAIL"
	}
}

// FailedClauses returns the clauses of every check that did NOT pass (for a
// concise failure summary / a test assertion).
func (rep Report) FailedClauses() []string {
	var out []string
	for _, r := range rep.Results {
		if !r.Pass {
			out = append(out, r.Clause)
		}
	}
	sort.Strings(out)
	return out
}
