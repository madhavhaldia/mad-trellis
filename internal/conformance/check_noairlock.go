package conformance

import (
	"fmt"
	"strings"
)

// check_noairlock.go proves the NO-AIRLOCK CLOSED-LOOP GUARANTEE (docs/0003 §10a
// "the Inv-12 closed-loop GUARANTEE (no-airlock)"; Inv 12, the load-bearing closed
// loop — intake side owned by session-launcher-shim + output side by
// integrator-trunk, the GUARANTEE conformance-checked here). The governed
// produce->consume path completes IN PLACE through the governed surface with NO
// manual app-boundary handoff (no copy-paste, no human moving an artifact across
// an app boundary). The loop is: claim/lease -> edit -> request-merge (push the
// nm/* branch to the mediated remote) -> submit -> integrate/promote -> the
// produced work is CONSUMED on the trunk.
//
// BLACK BOX over the public surface only. The probe drives the WHOLE loop through
// the governed surface (the mediated remote + the trunk CLI + the lease RPC) and
// asserts the produced bytes arrive on the trunk WITHOUT any out-of-band step:
//   1. claim: acquire the trunk lease (the produce side holds exclusivity);
//   2. edit: author a uniquely-marked file in the agent's worktree;
//   3. request-merge: push nm/* to the MEDIATED remote (the governed channel);
//   4. submit + promote: the integrator consumes it onto the trunk;
//   5. consume: the marked bytes are present on the trunk tree (the loop CLOSED in
//      place — the consumer received exactly what the producer made, via the
//      governed surface, with no manual handoff).
//
// CONTROL (non-vacuity): an injected APP-BOUNDARY HANDOFF must break the loop /
// flip RED. The control models the airlock anti-pattern: the producer authors the
// SAME work but only in its LOCAL working repo and DOES NOT push it through the
// governed mediated channel (the "copy-paste across an app boundary" step is
// required for the consumer to see it). It asserts the trunk does NOT receive the
// work — i.e. WITHOUT the governed channel the loop does NOT close, which is
// exactly why a manual handoff would be needed and why the governed loop's
// in-place closure is the guarantee. If the trunk somehow received the un-pushed
// work, the "closed in place via the governed surface" claim would be vacuous
// (the bytes would arrive by some other, ungoverned path).

func init() { RegisterCheck(noAirlockClosedLoop{}) }

type noAirlockClosedLoop struct{}

func (noAirlockClosedLoop) ID() string { return "no-airlock-closed-loop" }
func (noAirlockClosedLoop) OwnerProject() string {
	return "session-launcher-shim + integrator-trunk (Inv-12 GUARANTEE)"
}
func (noAirlockClosedLoop) Clause() string {
	return "no-airlock (Inv 12): the produce->consume loop closes IN PLACE through the governed surface, no manual app-boundary handoff"
}

// airlockMarker uniquely tags the produced work so its presence on the trunk tree
// proves the SAME bytes were consumed (not coincidental content).
const airlockMarker = "mad-trellis-CLOSED-LOOP-MARKER-7f3a"

func (c noAirlockClosedLoop) Run(s *Scratch) Result {
	agent, err := s.NewAgent("loop")
	if err != nil {
		return fail(c, "new agent: %v", err)
	}
	// Born trunk (the loop needs a base to integrate against).
	base, err := s.EstablishTrunkBase(agent, map[string]string{"a.txt": "base\n"})
	if err != nil {
		return fail(c, "establish trunk base: %v", err)
	}

	// 1) CLAIM: the producer acquires the trunk lease (exclusivity for the convergent
	// write) — part of the governed loop, not an external coordination step.
	key, ok, err := s.RouteLeaseKey("trunk", "")
	if err != nil || !ok {
		return fail(c, "route trunk key: ok=%v err=%v", ok, err)
	}
	claimer, err := s.Dial()
	if err != nil {
		return fail(c, "claimer dial: %v", err)
	}
	defer claimer.Close()
	var acq struct {
		Granted bool `json:"granted"`
	}
	if err := claimer.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 120000}, &acq); err != nil {
		return fail(c, "claim lease: %v", err)
	}
	if !acq.Granted {
		return fail(c, "producer could not claim the trunk lease")
	}
	// Release the claim before the integrator's own promote (the integrator acquires
	// the trunk lease itself; this models the agent's claim-then-hand-to-integrator
	// within ONE governed flow, no app boundary).
	var rel struct {
		OK bool `json:"ok"`
	}
	if err := claimer.Call("lease.release", map[string]any{"key": key}, &rel); err != nil {
		return fail(c, "release claim: %v", err)
	}

	// 2) EDIT: author uniquely-marked work in the agent's worktree.
	if err := agent.Checkout("loop-wb", "origin/trunk"); err != nil {
		return fail(c, "checkout: %v", err)
	}
	if _, err := agent.Commit("produced work", map[string]string{"produced.txt": airlockMarker + "\n"}); err != nil {
		return fail(c, "commit produced work: %v", err)
	}

	// 3) REQUEST-MERGE: push nm/* to the MEDIATED remote (the governed channel) —
	// NOT a manual copy to some other location.
	ref, err := agent.PushBranch("loop")
	if err != nil {
		return fail(c, "request-merge push: %v", err)
	}

	// 4) SUBMIT + PROMOTE: the integrator consumes it onto the trunk, in place.
	if _, err := s.SubmitAndPromote(ref); err != nil {
		return fail(c, "integrate/promote: %v", err)
	}

	// 5) CONSUME: the produced bytes are on the trunk tree (the loop CLOSED in place).
	tip, err := s.TrunkTip()
	if err != nil {
		return fail(c, "trunk tip: %v", err)
	}
	if tip == base {
		return fail(c, "the trunk did not advance — the loop did not close")
	}
	if !s.commitHasFile(tip, "produced.txt") {
		return fail(c, "AIRLOCK: the produced file never reached the trunk through the governed surface")
	}
	content, gerr := s.Git(s.BareDir, "show", tip+":produced.txt")
	if gerr != nil {
		return fail(c, "read consumed file from trunk: %v: %s", gerr, content)
	}
	if !strings.Contains(content, airlockMarker) {
		return fail(c, "AIRLOCK: the consumed file does not carry the producer's marker (bytes did not flow in place): %q", strings.TrimSpace(content))
	}

	return pass(c, "loop closed in place via the governed surface (claim->edit->request-merge->promote): producer's marked bytes consumed on trunk %s, no manual handoff",
		short12(tip))
}

func (c noAirlockClosedLoop) Control(s *Scratch) error {
	// INJECT the airlock anti-pattern (fix #10): MODEL a copy-paste / app-boundary
	// handoff by attempting to make the produced work reach trunk via an UNGOVERNED
	// path — a raw `git push` straight to the trunk ref (the out-of-band ref write a
	// human "copying the artifact across an app boundary" would attempt) — and assert
	// that path is REFUSED and does NOT advance trunk. This proves the ONLY in-place
	// path to trunk is the governed loop (the prior control merely OMITTED the push,
	// asserting a tautology; this control actively tries the ungoverned handoff).
	agent, err := s.NewAgent("loop-ctl")
	if err != nil {
		return fmt.Errorf("control new agent: %w", err)
	}
	base, err := s.EstablishTrunkBase(agent, map[string]string{"a.txt": "base\n"})
	if err != nil {
		return fmt.Errorf("control establish trunk: %w", err)
	}

	// EDIT locally.
	if err := agent.Checkout("ctl-loop-wb", "origin/trunk"); err != nil {
		return fmt.Errorf("control checkout: %w", err)
	}
	if _, err := agent.Commit("airlocked work", map[string]string{"airlocked.txt": airlockMarker + "\n"}); err != nil {
		return fmt.Errorf("control commit: %w", err)
	}

	// THE INJECTED UNGOVERNED HANDOFF: a raw push straight at the protected trunk ref
	// (bypassing submit/promote — the app-boundary copy-paste). It MUST be refused by
	// the trunk-protect hook (integrator-only) and leave the trunk byte-identical.
	out, perr := s.Git(agent.Dir, "push", "origin", "HEAD:refs/heads/trunk")
	if perr == nil {
		return fmt.Errorf("CONTROL FAILED TO FIRE: a raw ungoverned push to the trunk ref SUCCEEDED — an app-boundary handoff CAN smuggle work onto trunk, so 'closed in place via the governed loop' is false: %s", strings.TrimSpace(out))
	}
	if !strings.Contains(out, "integrator-only") {
		return fmt.Errorf("CONTROL: the ungoverned push rejection must name the integrator-only policy; got: %s", strings.TrimSpace(out))
	}

	// The trunk must be unchanged (still at base) and must NOT carry the airlocked file
	// — the ungoverned handoff did not advance trunk by any path.
	tip, err := s.TrunkTip()
	if err != nil {
		return fmt.Errorf("control trunk tip: %w", err)
	}
	if tip != base {
		return fmt.Errorf("CONTROL: the trunk advanced via an ungoverned path (tip %s != base %s)", short12(tip), short12(base))
	}
	if s.commitHasFile(tip, "airlocked.txt") {
		return fmt.Errorf("CONTROL: the airlocked file reached trunk via an ungoverned path — the ONLY in-place path is NOT the governed loop")
	}
	return nil
}
