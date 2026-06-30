package conformance

import (
	"fmt"
	"strings"
)

// check_e2e.go is the two-agent / isolated-worktree / one-trunk-lease /
// mediated-integration E2E ACCEPTANCE GATE (docs/0003 §10a "the ... E2E
// acceptance gate"; docs/0004 §10a). It is the end-to-end self-hosting-day
// scenario: two agents work in isolated boundaries, both push to the mediated
// trunk, and the convergent write serializes on the SINGLE trunk lease so exactly
// one advances at a time — one promote lands, the other (held off by the lease)
// is BLOCKED until the first completes, then lands on top. All black-box through
// the public CLI + the public lease RPC + the observable trunk ref.
//
// CONTROL: the blocked agent must EVENTUALLY land once the lease frees — a
// permanently-stuck second promote would make "one blocked" a vacuous deadlock
// rather than a serialized advance. Control asserts the second integration
// reaches promoted after the lease is released.

func init() { RegisterCheck(e2eAcceptance{}) }

type e2eAcceptance struct{}

func (e2eAcceptance) ID() string           { return "e2e-acceptance" }
func (e2eAcceptance) OwnerProject() string { return "conformance-harness (E2E gate)" }
func (e2eAcceptance) Clause() string {
	return "E2E acceptance: two isolated agents, one trunk lease, mediated integration, one blocked then serialized"
}

func (c e2eAcceptance) Run(s *Scratch) Result {
	// Two agents in isolated working repos, both origin -> the mediated bare trunk.
	a1, err := s.NewAgent("e2e1")
	if err != nil {
		return fail(c, "new agent 1: %v", err)
	}
	a2, err := s.NewAgent("e2e2")
	if err != nil {
		return fail(c, "new agent 2: %v", err)
	}
	if a1.Dir == a2.Dir {
		return fail(c, "the two agents share a working dir (not isolated)")
	}

	// Establish trunk via agent 1's base commit (mediated, born trunk).
	base, err := s.EstablishTrunkBase(a1, map[string]string{"shared.txt": "base\n"})
	if err != nil {
		return fail(c, "establish trunk base: %v", err)
	}

	// Both agents author NON-conflicting feature branches off trunk.
	if err := a1.Checkout("wb1", "origin/trunk"); err != nil {
		return fail(c, "agent1 checkout: %v", err)
	}
	if _, err := a1.Commit("agent1 feature", map[string]string{"one.txt": "from-agent-1\n"}); err != nil {
		return fail(c, "agent1 commit: %v", err)
	}
	ref1, err := a1.PushBranch("e2e-one")
	if err != nil {
		return fail(c, "agent1 push: %v", err)
	}
	if err := a2.Checkout("wb2", "origin/trunk"); err != nil {
		return fail(c, "agent2 checkout: %v", err)
	}
	if _, err := a2.Commit("agent2 feature", map[string]string{"two.txt": "from-agent-2\n"}); err != nil {
		return fail(c, "agent2 commit: %v", err)
	}
	ref2, err := a2.PushBranch("e2e-two")
	if err != nil {
		return fail(c, "agent2 push: %v", err)
	}

	id1, err := s.Submit(ref1)
	if err != nil {
		return fail(c, "submit agent1: %v", err)
	}
	id2, err := s.Submit(ref2)
	if err != nil {
		return fail(c, "submit agent2: %v", err)
	}

	// One trunk lease serializes the convergent write. #14 — SCOPE: the integrator
	// acquires the trunk lease per-promote and releases it on completion, so it never
	// holds a lease "during" a wait we could observe between two promotes (the v1 CAS
	// is fail-fast, not a queue). We therefore model genuine SINGLE-LEASE contention
	// explicitly: a holder session takes THE trunk lease (the same classifier-routed
	// key the integrator's promote acquires), and we ASSERT the holder identity over
	// lease.list — proving agent2's promote is blocked by a real hold on the one trunk
	// lease, not by a synthetic/unrelated mechanism. The key comes only from
	// classify.route (never fabricated).
	key, ok, err := s.RouteLeaseKey("trunk", "")
	if err != nil || !ok {
		return fail(c, "route trunk lease key: ok=%v err=%v", ok, err)
	}
	holder, err := s.Dial()
	if err != nil {
		return fail(c, "lease-holder dial: %v", err)
	}
	defer holder.Close()
	holderSession, err := whoAmIOn(holder)
	if err != nil {
		return fail(c, "holder whoami: %v", err)
	}
	var acq struct {
		Granted bool `json:"granted"`
	}
	if err := holder.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 60000}, &acq); err != nil {
		return fail(c, "hold trunk lease: %v", err)
	}
	if !acq.Granted {
		return fail(c, "could not hold the trunk lease to model the blocked agent")
	}

	// Assert the trunk lease is held by EXACTLY our holder session for THE routed key
	// — the contention is a real hold on the one trunk lease (#14), observable state.
	if h, n := s.leaseHolderFor(key); n != 1 || h != holderSession {
		return fail(c, "the trunk lease must be held by exactly our holder session %q (got holder=%q, count=%d) — contention not on the single trunk lease", short12(holderSession), short12(h), n)
	}

	// AGENT 2 promote is BLOCKED (the single lease is held): refuses, retryable,
	// trunk untouched. The integrator's own promote tries to acquire THE SAME key and
	// is refused by the CAS — that is the single-writer serialization.
	blocked := s.CLI("trunk", "promote", id2)
	if strings.Contains(blocked.Out(), "promoted") {
		return fail(c, "FAIL-OPEN: agent2 promoted while the single trunk lease was held: %s", blocked.Out())
	}
	if !strings.Contains(blocked.Out(), "retryable") {
		return fail(c, "the blocked agent's promote must be retryable; got: %s", blocked.Out())
	}
	if tip, _ := s.TrunkTip(); tip != base {
		return fail(c, "the blocked promote moved trunk %s -> %s (must be byte-identical)", short12(base), short12(tip))
	}

	// Release the held lease => AGENT 1 lands (acquires the now-free lease). id1 was
	// already submitted above, so we promote BY ID (no duplicate submit).
	var rel struct {
		OK bool `json:"ok"`
	}
	if err := holder.Call("lease.release", map[string]any{"key": key}, &rel); err != nil {
		return fail(c, "release trunk lease: %v", err)
	}
	res1 := s.CLI("trunk", "promote", id1)
	if !res1.OK() || !strings.Contains(res1.Out(), "promoted") {
		return fail(c, "agent1 must land once the lease is free; got exit %d: %s", res1.ExitCode, res1.Out())
	}
	tipAfter1, _ := s.TrunkTip()
	if tipAfter1 == base {
		return fail(c, "trunk did not advance after agent1 landed")
	}

	// AGENT 2 now lands ON TOP (stale-base => integrator aborts with a retryable
	// "resubmit"; agent2 resubmits against the new trunk and lands). This is the
	// serialized-advance: one blocked, then both land in order, never concurrently.
	res2 := s.CLI("trunk", "promote", id2)
	if strings.Contains(res2.Out(), "promoted") {
		// Lucky path: the mediated merge of two disjoint files still applied on top.
	} else {
		// Expected v1 path: trunk advanced under agent2 => resubmit against new tip.
		if err := a2.Checkout("wb2b", "origin/trunk"); err != nil {
			return fail(c, "agent2 resubmit checkout: %v", err)
		}
		if _, err := a2.Commit("agent2 feature v2", map[string]string{"two.txt": "from-agent-2\n"}); err != nil {
			return fail(c, "agent2 resubmit commit: %v", err)
		}
		ref2b, err := a2.PushBranch("e2e-two-b")
		if err != nil {
			return fail(c, "agent2 resubmit push: %v", err)
		}
		if _, err := s.SubmitAndPromote(ref2b); err != nil {
			return fail(c, "agent2 must land on top after resubmit: %v", err)
		}
	}
	tipAfter2, _ := s.TrunkTip()
	if tipAfter2 == tipAfter1 {
		return fail(c, "agent2 never advanced trunk past agent1's landing")
	}

	// Final: trunk carries BOTH agents' files (mediated integration of both).
	if !s.commitHasFile(tipAfter2, "one.txt") || !s.commitHasFile(tipAfter2, "two.txt") {
		return fail(c, "the final trunk tree must contain both agents' files (one.txt + two.txt)")
	}

	return pass(c, "two isolated agents serialized on one trunk lease: agent2 blocked while held, agent1 landed %s->%s, agent2 landed %s; trunk carries both files",
		short12(base), short12(tipAfter1), short12(tipAfter2))
}

func (c e2eAcceptance) Control(s *Scratch) error {
	// INJECT the contention the E2E's serialization clause is about and assert the
	// check's OWN blocking predicate fires (fix #7 — NOT a re-run of Run). With the
	// single trunk lease HELD, a promote MUST be blocked (not promoted + retryable +
	// trunk byte-identical); releasing it MUST let the SAME integration land. If a
	// regression fail-OPENED (promoted under a held lease) this control fails; if the
	// serialization deadlocked (never lands after release) this control also fails.
	agent, err := s.NewAgent("e2e-ctl")
	if err != nil {
		return fmt.Errorf("control new agent: %w", err)
	}
	base, err := s.EstablishTrunkBase(agent, map[string]string{"shared.txt": "base\n"})
	if err != nil {
		return fmt.Errorf("control establish trunk: %w", err)
	}
	if err := agent.Checkout("e2e-ctl-wb", "origin/trunk"); err != nil {
		return fmt.Errorf("control checkout: %w", err)
	}
	if _, err := agent.Commit("e2e-ctl feature", map[string]string{"one.txt": "x\n"}); err != nil {
		return fmt.Errorf("control commit: %w", err)
	}
	ref, err := agent.PushBranch("e2e-ctl")
	if err != nil {
		return fmt.Errorf("control push: %w", err)
	}
	id, err := s.Submit(ref)
	if err != nil {
		return fmt.Errorf("control submit: %w", err)
	}

	// INJECT: a rival holds the single trunk lease.
	key, ok, err := s.RouteLeaseKey("trunk", "")
	if err != nil || !ok {
		return fmt.Errorf("control route key: ok=%v err=%v", ok, err)
	}
	holder, err := s.Dial()
	if err != nil {
		return fmt.Errorf("control holder dial: %w", err)
	}
	defer holder.Close()
	var acq struct {
		Granted bool `json:"granted"`
	}
	if err := holder.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 60000}, &acq); err != nil {
		return fmt.Errorf("control acquire: %w", err)
	}
	if !acq.Granted {
		return fmt.Errorf("control could not hold the trunk lease")
	}

	// Assert the blocking predicate FIRES.
	blocked := s.CLI("trunk", "promote", id)
	if strings.Contains(blocked.Out(), "promoted") {
		return fmt.Errorf("CONTROL FAILED TO FIRE: a promote SUCCEEDED while the single trunk lease was held (fail-open not caught): %s", blocked.Out())
	}
	if !strings.Contains(blocked.Out(), "retryable") {
		return fmt.Errorf("CONTROL: the blocked promote must be retryable; got: %s", blocked.Out())
	}
	if tip, _ := s.TrunkTip(); tip != base {
		return fmt.Errorf("CONTROL: the blocked promote moved trunk %s -> %s (must be byte-identical)", short12(base), short12(tip))
	}

	// Release → the SAME integration lands (serialization made progress, no deadlock).
	var rel struct {
		OK bool `json:"ok"`
	}
	if err := holder.Call("lease.release", map[string]any{"key": key}, &rel); err != nil {
		return fmt.Errorf("control release: %w", err)
	}
	res := s.CLI("trunk", "promote", id)
	if !res.OK() || !strings.Contains(res.Out(), "promoted") {
		return fmt.Errorf("CONTROL VACUOUS: after release the blocked integration must land (deadlock, not serialization): exit %d %s", res.ExitCode, res.Out())
	}
	return nil
}

// commitHasFile reports whether the trunk commit's tree contains a path (observable
// state via raw git on the mediated bare repo).
func (s *Scratch) commitHasFile(commit, path string) bool {
	return s.GitOK(s.BareDir, "cat-file", "-e", commit+":"+path)
}
