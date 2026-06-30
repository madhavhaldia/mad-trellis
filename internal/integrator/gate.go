package integrator

// GateResult is the deterministic verdict of a validation gate. OK gates the
// promote: an integration NEVER reaches `promoted` without a gate firing and
// recording a pass (Inv 6 "nothing merges silently"). Tree is the merged tree
// OID a passing merge produced (consumed to build the merge commit), so the gate
// and the promote agree on exactly one tree — no re-merge, no silent drift.
type GateResult struct {
	OK     bool
	Reason string // why it failed (conflict summary); empty on pass
	Tree   string // merged tree OID on pass; empty on fail
}

// ValidationGate is the validated-integration seam (Inv 6). v1 ships ONE
// deterministic implementation (a clean-merge check); richer gates (run the
// test suite, lint, policy) are the Layer-2 plug-in point — this interface is
// the cut, deliberately EMPTY of any such gate in v1. A gate MUST be a pure
// deterministic function of the repo contents at (trunkOID, branchOID): no
// probabilistic/heuristic/LLM component may sit on the promote path (Inv 2(b)),
// which the conformance harness (10a) asserts across the whole lock path.
type ValidationGate interface {
	// Validate decides whether branchOID may merge onto trunkOID. trunkOID is
	// empty for an unborn trunk (first promote). It MUST NOT mutate any ref.
	Validate(t *trunkRepo, trunkOID, branchOID string) (GateResult, error)
}

// mergeGate is the v1 gate: an integration passes iff the branch merges cleanly
// onto the trunk (no conflicts), decided by git's 3-way merge (merge-tree). It
// is fully deterministic — the same (trunk, branch) contents always yield the
// same verdict and tree — and writes only objects, never a ref.
type mergeGate struct{}

// Validate implements ValidationGate.
func (mergeGate) Validate(t *trunkRepo, trunkOID, branchOID string) (GateResult, error) {
	tree, clean, summary, err := t.mergeTree(trunkOID, branchOID)
	if err != nil {
		return GateResult{}, err
	}
	if !clean {
		return GateResult{OK: false, Reason: summary}, nil
	}
	return GateResult{OK: true, Tree: tree}, nil
}
