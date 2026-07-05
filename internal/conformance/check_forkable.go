package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// check_forkable.go proves the safety-property conjunct (a): no agent can observe
// or corrupt another's in-progress FORKABLE state, AND there is no cross-agent
// coordination channel (GROUNDING L138; docs/0003 §10a; clause owned by the
// conjunction, the forkable half rooted in isolation-substrate / Inv 1).
//
// BLACK BOX: it spawns two boundaries over the public `mad-trellis spawn` CLI and
// asserts, over OBSERVABLE state only (the reported cwd/branch/ports + the
// filesystem), that the two boundaries are disjoint (distinct cwd, distinct
// branch, disjoint port blocks, disjoint local-state roots) and that one agent's
// in-progress write into its own worktree is INVISIBLE in the other's worktree.
// The coordination-channel half: the two agents' state roots do not overlap, so
// there is no shared writable forkable path one could use as a side channel.
//
// CONTROL (non-vacuity): two boundaries that SHARED a cwd would make the
// cross-visibility assertion false. The control simulates that by writing a file
// into agent-1's cwd and checking the SAME path inside agent-1 (a deliberately
// shared self-comparison) is visible — proving the visibility probe actually
// detects content when the dirs DO overlap, so its negative result for two
// distinct dirs is meaningful.

func init() { RegisterCheck(forkableIsolation{}) }

type forkableIsolation struct{}

func (forkableIsolation) ID() string           { return "forkable-isolation" }
func (forkableIsolation) OwnerProject() string { return "isolation-substrate (conjunction)" }
func (forkableIsolation) Clause() string {
	return "safety (a): no cross-visibility of forkable state + no cross-agent coordination channel (Inv 1)"
}

func (c forkableIsolation) Run(s *Scratch) Result {
	a, _, err := s.Spawn()
	if err != nil {
		return fail(c, "spawn agent-1: %v", err)
	}
	b, _, err := s.Spawn()
	if err != nil {
		return fail(c, "spawn agent-2: %v", err)
	}

	// Distinct worktree cwd, distinct branch, distinct session.
	if a.Cwd == b.Cwd {
		return fail(c, "two agents share a worktree cwd %q (cross-visible forkable FS)", a.Cwd)
	}
	if a.Branch == b.Branch {
		return fail(c, "two agents share a branch %q (not single-writer per agent)", a.Branch)
	}
	if a.Session == b.Session {
		return fail(c, "two distinct connections minted the SAME session %q (identity not per-agent)", a.Session)
	}

	// Disjoint port blocks (Inv 1 explicitly covers ports).
	if shared := intersect(a.Ports, b.Ports); len(shared) > 0 {
		return fail(c, "two agents share ports %v (forkable runtime not isolated)", shared)
	}

	// Disjoint local-state roots: neither cwd is nested in the other (no shared
	// writable forkable path = no filesystem coordination channel).
	if nested(a.Cwd, b.Cwd) || nested(b.Cwd, a.Cwd) {
		return fail(c, "one agent's cwd is nested in the other's: %q vs %q", a.Cwd, b.Cwd)
	}

	// In-progress write isolation: a file agent-1 writes into ITS worktree must be
	// invisible in agent-2's worktree (an agent's in-progress work is invisible to
	// every other agent except through the integration plane).
	secret := filepath.Join(a.Cwd, "in-progress-secret.txt")
	if err := os.WriteFile(secret, []byte("agent-1 private\n"), 0o644); err != nil {
		return fail(c, "could not write agent-1 in-progress file: %v", err)
	}
	leaked := filepath.Join(b.Cwd, "in-progress-secret.txt")
	if _, err := os.Stat(leaked); err == nil {
		return fail(c, "agent-1's in-progress file leaked into agent-2's worktree at %q", leaked)
	} else if !os.IsNotExist(err) {
		return fail(c, "unexpected error checking leak path %q: %v", leaked, err)
	}

	// NO CROSS-AGENT COORDINATION CHANNEL (fix #9): the clause has TWO parts — FS
	// disjointness (above) AND the absence of any peer/session-to-session affordance.
	// Assert the second part black-box: every coordination-shaped method is ABSENT
	// from the public registry (method-not-found, -32601), and two distinct sessions
	// cannot observe each other beyond the integration plane.
	if r := c.assertNoCoordinationChannel(s); !r.Pass {
		return r
	}

	return pass(c, "two boundaries disjoint: cwd %s|%s, branch %s|%s, ports %v|%v; in-progress write did not cross; "+
		"no cross-agent coordination affordance (%v all method-not-found)",
		filepath.Base(a.Cwd), filepath.Base(b.Cwd), a.Branch, b.Branch, a.Ports, b.Ports, coordinationMethods)
}

// coordinationMethods are peer/session-to-session affordances that MUST NOT exist
// in the public registry — their presence would be a cross-agent coordination
// channel (a star-not-mesh violation). Each must return method-not-found (-32601).
var coordinationMethods = []string{
	"session.send", "session.list", "session.broadcast", "session.recv",
	"relay.send", "relay.recv", "broadcast.publish", "broadcast.subscribe",
	"peer.connect", "peer.send", "peer.list", "mesh.join", "mesh.send", "channel.open",
}

// assertNoCoordinationChannel proves the second half of conjunct (a): the public
// surface exposes NO peer/session-to-session affordance. Each coordination-shaped
// method must be method-not-found, and two distinct sessions must not be able to
// observe each other's identity through any of them (no mesh).
func (c forkableIsolation) assertNoCoordinationChannel(s *Scratch) Result {
	cl, err := s.Dial()
	if err != nil {
		return fail(c, "coordination dial: %v", err)
	}
	defer cl.Close()
	for _, m := range coordinationMethods {
		var out map[string]any
		err := cl.Call(m, map[string]any{}, &out)
		if err == nil {
			return fail(c, "COORDINATION CHANNEL: the registry served %q (a cross-agent affordance exists — mesh, not star)", m)
		}
		if !isMethodNotFound(err) {
			return fail(c, "coordination method %q returned a non-not-found error (it may be partially wired): %v", m, err)
		}
	}
	// Two distinct sessions: each can learn ONLY its OWN identity (session.whoami is
	// connection-bound). There is no method to enumerate or message the other — the
	// only shared plane is the integration plane (trunk), tested elsewhere.
	s1, err := s.WhoAmI()
	if err != nil {
		return fail(c, "session 1 whoami: %v", err)
	}
	s2, err := s.WhoAmI()
	if err != nil {
		return fail(c, "session 2 whoami: %v", err)
	}
	if s1 == s2 {
		return fail(c, "two distinct connections minted the SAME session %q (identity not per-agent)", s1)
	}
	return pass(c, "no coordination channel")
}

func (c forkableIsolation) Control(s *Scratch) error {
	// (1) The visibility probe must DETECT content when two paths DO overlap —
	// otherwise its "not visible" verdict for two distinct dirs is vacuous. Spawn one
	// agent, write a file into its cwd, and confirm the same path (a shared self-dir)
	// IS visible.
	a, _, err := s.Spawn()
	if err != nil {
		return fmt.Errorf("control spawn: %w", err)
	}
	probe := filepath.Join(a.Cwd, "control-visible.txt")
	if err := os.WriteFile(probe, []byte("x\n"), 0o644); err != nil {
		return fmt.Errorf("control write: %w", err)
	}
	if _, err := os.Stat(probe); err != nil {
		return fmt.Errorf("CONTROL VACUOUS: a file written into a worktree is not even visible in that same worktree: %v", err)
	}

	// (2) The coordination-channel detector must DISTINGUISH a PRESENT method from an
	// ABSENT one — otherwise "all coordination methods absent" is vacuously true. A
	// known-PRESENT method (session.whoami) must NOT look like method-not-found, and a
	// known-ABSENT one (a fabricated coordination method) MUST. If the probe can't
	// tell present from absent, the Run's "no coordination channel" verdict is hollow.
	cl, err := s.Dial()
	if err != nil {
		return fmt.Errorf("control dial: %w", err)
	}
	defer cl.Close()
	var who map[string]any
	if err := cl.Call("session.whoami", map[string]any{}, &who); err != nil {
		return fmt.Errorf("CONTROL VACUOUS: a known-present method (session.whoami) failed (%v) — the method-probe cannot confirm presence", err)
	}
	var absent map[string]any
	aerr := cl.Call("session.send", map[string]any{}, &absent)
	if aerr == nil {
		return fmt.Errorf("CONTROL: a coordination affordance (session.send) was unexpectedly SERVED — the detector's absent-case baseline is invalid")
	}
	if !isMethodNotFound(aerr) {
		return fmt.Errorf("CONTROL VACUOUS: a fabricated coordination method did not return method-not-found (%v) — the detector cannot tell absent from present", aerr)
	}
	return nil
}

// intersect returns the ports common to a and b.
func intersect(a, b []int) []int {
	set := map[int]bool{}
	for _, p := range a {
		set[p] = true
	}
	var out []int
	for _, p := range b {
		if set[p] {
			out = append(out, p)
		}
	}
	return out
}

// nested reports whether child is at or inside parent (path containment).
func nested(parent, child string) bool {
	pa, err1 := filepath.Abs(parent)
	ca, err2 := filepath.Abs(child)
	if err1 != nil || err2 != nil {
		return false
	}
	if pa == ca {
		return true
	}
	rel, err := filepath.Rel(pa, ca)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
