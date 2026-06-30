package conformance

import (
	"fmt"
	"strings"
)

// check_convergent.go proves the safety-property conjunct (b): no agent can write
// a CONVERGENT resource (the trunk) without an exclusive lease AND validated
// integration (GROUNDING L139; docs/0003 §10a; the trunk path is owned by
// integrator-trunk, the single-writer half by the lease).
//
// It carries BOTH negatives the brief requires, each black-box through the public
// CLI + the public lease RPC + the observable trunk ref:
//
//	NEGATIVE 1 (lease) — a RIVAL session holds the trunk lease (acquired via the
//	  public lease.acquire over the classifier-routed key); a promote then REFUSES
//	  (retryable "trunk busy") and the trunk ref is byte-identical. Releasing the
//	  rival lease lets the SAME integration promote — proving the refusal was the
//	  lease, not an unrelated failure.
//
//	NEGATIVE 2 (validation) — a branch that CONFLICTS with trunk is gate-REFUSED
//	  (aborted, never promoted) and the trunk ref is untouched (nothing merges
//	  silently).
//
//	POSITIVE CONTROL — a clean branch with a FREE lease promotes and the trunk ref
//	  advances; so neither negative is vacuously rejecting everything.
//
// The single Run composes all three so a green Result means "trunk advances iff
// (clean AND lease-free)". Control re-asserts non-vacuity by disabling the lease
// hold and confirming the contended integration THEN promotes.

func init() { RegisterCheck(convergentGated{}) }

type convergentGated struct{}

func (convergentGated) ID() string           { return "trunk-lease-and-validated" }
func (convergentGated) OwnerProject() string { return "integrator-trunk + lease-ledger-mutex" }
func (convergentGated) Clause() string {
	return "safety (b): no convergent write without an exclusive lease AND validated integration (Inv 6/7)"
}

func (c convergentGated) Run(s *Scratch) Result {
	agent, err := s.NewAgent("conv")
	if err != nil {
		return fail(c, "new agent: %v", err)
	}

	// Establish a trunk base (born trunk) over the public path.
	base, err := s.EstablishTrunkBase(agent, map[string]string{"a.txt": "base\n"})
	if err != nil {
		return fail(c, "establish trunk base: %v", err)
	}
	if base == "" {
		return fail(c, "trunk did not come into being after the base promote")
	}

	// --- POSITIVE CONTROL: clean branch + free lease => promotes, trunk advances.
	if err := agent.Checkout("wb-clean", "origin/trunk"); err != nil {
		return fail(c, "checkout clean: %v", err)
	}
	if _, err := agent.Commit("clean feature", map[string]string{"b.txt": "added\n"}); err != nil {
		return fail(c, "commit clean: %v", err)
	}
	cleanRef, err := agent.PushBranch("clean")
	if err != nil {
		return fail(c, "push clean: %v", err)
	}
	if _, err := s.SubmitAndPromote(cleanRef); err != nil {
		return fail(c, "clean branch should promote with a free lease: %v", err)
	}
	afterClean, err := s.TrunkTip()
	if err != nil {
		return fail(c, "trunk tip after clean: %v", err)
	}
	if afterClean == base {
		return fail(c, "POSITIVE CONTROL DEAD: a clean branch did not advance trunk (gate vacuously rejects all)")
	}

	// --- NEGATIVE 1 (lease): a rival holds the trunk lease => promote refuses,
	// trunk untouched; release => the same integration promotes.
	if r := c.checkLeaseGate(s, agent, afterClean); !r.Pass {
		return r
	}

	// --- NEGATIVE 2 (validation): a conflicting branch is gate-refused, trunk
	// untouched.
	if r := c.checkValidationGate(s, agent); !r.Pass {
		return r
	}

	return pass(c, "trunk advances iff (clean+lease-free): clean promoted %s..%s; rival-lease refused & trunk held; conflict aborted & trunk held",
		short12(base), short12(afterClean))
}

// checkLeaseGate is NEGATIVE 1: with the trunk lease held by a RIVAL session, a
// promote must refuse and leave trunk byte-identical; releasing it lets the same
// integration promote (proving the lease, not luck, was the gate).
func (c convergentGated) checkLeaseGate(s *Scratch, agent *Agent, trunkBefore string) Result {
	// A fresh agent branch ready to promote.
	if err := agent.Checkout("wb-leased", "origin/trunk"); err != nil {
		return fail(c, "checkout leased: %v", err)
	}
	if _, err := agent.Commit("leased feature", map[string]string{"c.txt": "leased\n"}); err != nil {
		return fail(c, "commit leased: %v", err)
	}
	ref, err := agent.PushBranch("leased")
	if err != nil {
		return fail(c, "push leased: %v", err)
	}
	id, err := s.Submit(ref)
	if err != nil {
		return fail(c, "submit leased: %v", err)
	}

	// The RIVAL session holds the trunk lease via the PUBLIC lease surface. The key
	// comes ONLY from classify.route (never fabricated).
	key, ok, err := s.RouteLeaseKey("trunk", "")
	if err != nil || !ok {
		return fail(c, "could not route the trunk lease key: ok=%v err=%v", ok, err)
	}
	rival, err := s.Dial()
	if err != nil {
		return fail(c, "rival dial: %v", err)
	}
	defer rival.Close()
	var acq struct {
		Granted bool   `json:"granted"`
		Holder  string `json:"holder"`
	}
	if err := rival.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 60000}, &acq); err != nil {
		return fail(c, "rival lease.acquire: %v", err)
	}
	if !acq.Granted {
		return fail(c, "rival could not acquire the free trunk lease (setup failed): holder=%s", acq.Holder)
	}

	// Promote under the rival hold => must refuse (retryable), trunk untouched.
	res := s.CLI("trunk", "promote", id)
	if !res.OK() {
		return fail(c, "promote CLI errored under rival lease (want a clean retryable refusal): exit %d %s", res.ExitCode, res.Out())
	}
	if strings.Contains(res.Out(), "promoted") {
		return fail(c, "FAIL-OPEN: promoted while a rival held the trunk lease: %s", res.Out())
	}
	if !strings.Contains(res.Out(), "retryable") {
		return fail(c, "a lease-contended promote must report retryable; got: %s", res.Out())
	}
	if tip, _ := s.TrunkTip(); tip != trunkBefore {
		return fail(c, "a rival-held promote moved trunk %s -> %s (must be byte-identical)", short12(trunkBefore), short12(tip))
	}

	// Release the rival hold => the SAME integration now promotes (the gate WAS the
	// lease).
	var rel struct {
		OK bool `json:"ok"`
	}
	if err := rival.Call("lease.release", map[string]any{"key": key}, &rel); err != nil {
		return fail(c, "rival lease.release: %v", err)
	}
	res2 := s.CLI("trunk", "promote", id)
	if !res2.OK() || !strings.Contains(res2.Out(), "promoted") {
		return fail(c, "after the rival released, the same integration must promote; got exit %d: %s", res2.ExitCode, res2.Out())
	}
	if tip, _ := s.TrunkTip(); tip == trunkBefore {
		return fail(c, "CONTROL: trunk must advance once the lease is granted (stayed at %s)", short12(trunkBefore))
	}
	return pass(c, "lease-gated")
}

// checkValidationGate is NEGATIVE 2: a branch that conflicts with trunk is
// gate-refused (aborted, never promoted) and trunk is untouched.
func (c convergentGated) checkValidationGate(s *Scratch, agent *Agent) Result {
	trunkBefore, err := s.TrunkTip()
	if err != nil {
		return fail(c, "trunk tip before conflict: %v", err)
	}

	// Advance trunk so a.txt diverges, then branch off the ORIGINAL base editing the
	// same file => a real conflict. (nm/base still exists in the bare repo.)
	if err := agent.Checkout("wb-trunkchange", "origin/trunk"); err != nil {
		return fail(c, "checkout trunkchange: %v", err)
	}
	if _, err := agent.Commit("trunk change", map[string]string{"a.txt": "trunk-version\n"}); err != nil {
		return fail(c, "commit trunkchange: %v", err)
	}
	tcRef, err := agent.PushBranch("trunkchange")
	if err != nil {
		return fail(c, "push trunkchange: %v", err)
	}
	if _, err := s.SubmitAndPromote(tcRef); err != nil {
		return fail(c, "setup: trunkchange should promote cleanly: %v", err)
	}
	trunkAfterSetup, err := s.TrunkTip()
	if err != nil {
		return fail(c, "trunk tip after trunkchange: %v", err)
	}
	_ = trunkBefore

	// Conflicting branch off the original base, editing the SAME line.
	if err := agent.Checkout("wb-conflict", "origin/nm/base"); err != nil {
		return fail(c, "checkout conflict (off base): %v", err)
	}
	if _, err := agent.Commit("conflicting edit", map[string]string{"a.txt": "conflicting\n"}); err != nil {
		return fail(c, "commit conflict: %v", err)
	}
	cRef, err := agent.PushBranch("conflict")
	if err != nil {
		return fail(c, "push conflict: %v", err)
	}
	id, err := s.Submit(cRef)
	if err != nil {
		return fail(c, "submit conflict: %v", err)
	}
	res := s.CLI("trunk", "promote", id)
	if !res.OK() {
		return fail(c, "promote of a conflicting branch errored (want a clean abort): exit %d %s", res.ExitCode, res.Out())
	}
	if strings.Contains(res.Out(), "promoted") {
		return fail(c, "FAIL-OPEN: a conflicting branch PROMOTED (nothing merges silently violated): %s", res.Out())
	}
	if tip, _ := s.TrunkTip(); tip != trunkAfterSetup {
		return fail(c, "a gate-rejected integration moved trunk %s -> %s (must be byte-identical)", short12(trunkAfterSetup), short12(tip))
	}
	return pass(c, "validation-gated")
}

func (c convergentGated) Control(s *Scratch) error {
	// INJECT the negative this check is meant to catch and assert the check's OWN
	// negative predicate fires (fix #7 — NOT a re-run of Run). We hold the rival trunk
	// lease, attempt the promote, and assert the lease-gate predicate: NOT promoted +
	// retryable + trunk BYTE-IDENTICAL. Then we release and assert the SAME
	// integration promotes — proving the gate WAS the lease. This exercises the
	// assertion logic directly, so a regression that fail-OPENED (promoted under a
	// held lease) would fail THIS control, not just Run.
	agent, err := s.NewAgent("conv-ctl")
	if err != nil {
		return fmt.Errorf("control new agent: %w", err)
	}
	base, err := s.EstablishTrunkBase(agent, map[string]string{"a.txt": "base\n"})
	if err != nil {
		return fmt.Errorf("control establish trunk: %w", err)
	}
	if err := agent.Checkout("ctl-leased", "origin/trunk"); err != nil {
		return fmt.Errorf("control checkout: %w", err)
	}
	if _, err := agent.Commit("ctl leased feature", map[string]string{"c.txt": "leased\n"}); err != nil {
		return fmt.Errorf("control commit: %w", err)
	}
	ref, err := agent.PushBranch("ctl-leased")
	if err != nil {
		return fmt.Errorf("control push: %w", err)
	}
	id, err := s.Submit(ref)
	if err != nil {
		return fmt.Errorf("control submit: %w", err)
	}

	// INJECT: a rival holds the trunk lease.
	key, ok, err := s.RouteLeaseKey("trunk", "")
	if err != nil || !ok {
		return fmt.Errorf("control route key: ok=%v err=%v", ok, err)
	}
	rival, err := s.Dial()
	if err != nil {
		return fmt.Errorf("control rival dial: %w", err)
	}
	defer rival.Close()
	var acq struct {
		Granted bool `json:"granted"`
	}
	if err := rival.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 60000}, &acq); err != nil {
		return fmt.Errorf("control rival acquire: %w", err)
	}
	if !acq.Granted {
		return fmt.Errorf("control rival could not hold the trunk lease")
	}

	// Assert the check's OWN negative predicate FIRES: NOT promoted, retryable, trunk
	// byte-identical.
	res := s.CLI("trunk", "promote", id)
	if !res.OK() {
		return fmt.Errorf("control promote CLI errored under rival lease: exit %d %s", res.ExitCode, res.Out())
	}
	if strings.Contains(res.Out(), "promoted") {
		return fmt.Errorf("CONTROL FAILED TO FIRE: promote SUCCEEDED while a rival held the trunk lease (the lease-gate predicate did not catch the fail-open): %s", res.Out())
	}
	if !strings.Contains(res.Out(), "retryable") {
		return fmt.Errorf("CONTROL: a lease-contended promote must report retryable; got: %s", res.Out())
	}
	if tip, _ := s.TrunkTip(); tip != base {
		return fmt.Errorf("CONTROL: a rival-held promote moved trunk %s -> %s (must be byte-identical)", short12(base), short12(tip))
	}

	// Release → the SAME integration promotes (the gate WAS the lease, not a dead path).
	var rel struct {
		OK bool `json:"ok"`
	}
	if err := rival.Call("lease.release", map[string]any{"key": key}, &rel); err != nil {
		return fmt.Errorf("control rival release: %w", err)
	}
	res2 := s.CLI("trunk", "promote", id)
	if !res2.OK() || !strings.Contains(res2.Out(), "promoted") {
		return fmt.Errorf("CONTROL VACUOUS: after release the same integration must promote (else the gate rejects everything): exit %d %s", res2.ExitCode, res2.Out())
	}
	return nil
}

// short12 truncates an oid for readable detail strings.
func short12(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	if oid == "" {
		return "∅"
	}
	return oid
}
