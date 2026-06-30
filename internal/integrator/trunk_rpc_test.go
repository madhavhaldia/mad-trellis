package integrator

// integrate.trunk — the READ-ONLY authoritative trunk-ref method 9a's watch
// trunk panel mirrors. The point of the method (vs. deriving the tip from
// integrate.list) is that promote order is NOT created_at order: an OUT-OF-ORDER
// promote leaves two `promoted` rows where the one with the LATER created_at is
// the SUPERSEDED commit. integrate.trunk reports the commit the CAS last
// advanced — the real git ref — so the watch view can never assert a stale tip.

import (
	"encoding/json"
	"testing"
)

// callIntegrateTrunk invokes the integrate.trunk handler and decodes its result.
// trunkHandler ignores its CallContext, so a nil context is fine here.
func callIntegrateTrunk(t *testing.T, it *Integrator) (tip string, exists bool, branch string) {
	t.Helper()
	raw, perr := trunkHandler(it)(nil, nil)
	if perr != nil {
		t.Fatalf("integrate.trunk handler error: %+v", perr)
	}
	var out struct {
		Tip    string `json:"tip"`
		Exists bool   `json:"exists"`
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode integrate.trunk: %v", err)
	}
	return out.Tip, out.Exists, out.Branch
}

func TestIntegrateTrunkReturnsAuthoritativeTip(t *testing.T) {
	f := newFix(t)
	it := f.newIntegrator(newRealLeaseGate(t))

	// Baseline: the handler mirrors the established base trunk tip + branch name.
	tip0, exists0, branch0 := callIntegrateTrunk(t, it)
	if !exists0 || tip0 != f.trunkTip() {
		t.Fatalf("integrate.trunk must mirror the real trunk tip; got tip=%q exists=%v want=%q",
			tip0, exists0, f.trunkTip())
	}
	if branch0 != "trunk" {
		t.Fatalf("integrate.trunk branch: got %q want trunk", branch0)
	}

	// Submit A (earlier created_at) then B (later created_at), both off trunk with
	// DISJOINT files so each is a clean merge.
	brA := f.pushBranch("aaa", "origin/trunk", map[string]string{"fa.txt": "A\n"})
	recA, err := it.Submit("s-A", brA)
	if err != nil {
		t.Fatalf("submit A: %v", err)
	}
	brB := f.pushBranch("bbb", "origin/trunk", map[string]string{"fb.txt": "B\n"})
	recB, err := it.Submit("s-B", brB)
	if err != nil {
		t.Fatalf("submit B: %v", err)
	}

	// Promote B FIRST, then A. A is still `received`, so it re-reads the FRESH tip
	// (= B) as its base, validates against it, and advances trunk -> A. Both end
	// `promoted`; the real tip is A's merge commit (the LAST advance), while B has
	// the LATER created_at — exactly the case "last promoted in the list" gets wrong.
	outB, err := it.Promote("s-B", recB.ID)
	if err != nil || !outB.Promoted {
		t.Fatalf("promote B: out=%+v err=%v", outB, err)
	}
	outA, err := it.Promote("s-A", recA.ID)
	if err != nil || !outA.Promoted {
		t.Fatalf("promote A: out=%+v err=%v", outA, err)
	}

	realTip := f.trunkTip()
	tip, exists, _ := callIntegrateTrunk(t, it)
	if !exists {
		t.Fatal("integrate.trunk must report exists=true after promotion")
	}
	if tip != realTip {
		t.Fatalf("integrate.trunk must report the AUTHORITATIVE git tip %q; got %q", realTip, tip)
	}
	if tip != outA.TrunkTip {
		t.Fatalf("authoritative tip must be A's advance %q (the last CAS); got %q", outA.TrunkTip, tip)
	}
	if tip == outB.TrunkTip {
		t.Fatalf("integrate.trunk returned the SUPERSEDED B tip %q — the fidelity defect the method exists to avoid", tip)
	}
}
