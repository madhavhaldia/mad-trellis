package conformance

import (
	"fmt"
	"strings"
)

// check_nodispatch.go proves the NO-GOALS / NO-TASK-DISPATCH negative obligation
// (docs/0003 §10a "the no-goals/no-dispatch check"; Inv 13, owned by
// session-launcher-shim). mad-substrate is a governance SUBSTRATE, not an
// orchestrator: NO public method or CLI affordance accepts a GOAL, dispatches
// work, or triggers an agent. The task-intake seam is deliberately EMPTY.
//
// BLACK BOX over the public surface only:
//   - the CLI: `mad-substrate launch` (the closest thing to "run an agent") rejects a
//     --goal/--task/--prompt flag — the only inputs are the socket, a port count,
//     and the OPAQUE agent command after `--`, forwarded verbatim. No subcommand
//     takes a goal.
//   - the RPC: a dispatch-shaped method (agent.dispatch / task.create / goal.*) is
//     NOT in the registry — calling it returns method-not-found, never a dispatch.
//
// CONTROL (non-vacuity): a STUB dispatch affordance must flip the check RED — the
// control asserts that IF a goal/dispatch surface existed (an accepted --goal flag
// OR a registered dispatch method), the probe would detect it. We simulate the
// "would-be-RED" by asserting the detector fires on a KNOWN-present affordance (a
// real flag the CLI does accept, e.g. --socket) — proving the flag-probe is live —
// and that a registered method (e.g. diag.health) IS reachable — proving the
// method-probe distinguishes present from absent.

func init() { RegisterCheck(noDispatch{}) }

type noDispatch struct{}

func (noDispatch) ID() string           { return "no-goals-no-dispatch" }
func (noDispatch) OwnerProject() string { return "session-launcher-shim" }
func (noDispatch) Clause() string {
	return "no-goals/no-dispatch: no public method or CLI affordance accepts a goal / dispatches / triggers an agent (Inv 13)"
}

// dispatchFlags are goal/dispatch-shaped CLI flags that MUST NOT be accepted by
// any subcommand (their presence would mean mad-substrate schedules work).
var dispatchFlags = []string{"--goal", "--task", "--prompt", "--dispatch", "--objective", "--mission"}

// dispatchMethods are goal/dispatch-shaped RPC methods that MUST NOT be in the
// registry (calling one must be method-not-found, never an accepted dispatch).
var dispatchMethods = []string{"agent.dispatch", "task.create", "task.dispatch", "goal.set", "goal.submit", "orchestrate.run", "schedule.task"}

// goalProbeSubcommands are the agent-facing CLI surfaces a goal flag would most
// plausibly be smuggled into.
var goalProbeSubcommands = [][]string{
	{"launch"},
	{"spawn"},
	{"trunk", "submit"},
	{"gate", "request"},
}

func (c noDispatch) Run(s *Scratch) Result {
	// --- CLI: no subcommand accepts a goal/dispatch flag. cobra rejects an unknown
	// flag with a non-zero exit + an "unknown flag" message. We assert EACH probe
	// surface rejects EACH dispatch flag.
	for _, sub := range goalProbeSubcommands {
		for _, flag := range dispatchFlags {
			args := append(append([]string(nil), sub...), flag, "x")
			res := s.CLI(args...)
			// A dispatch flag must NOT be a recognized flag. If cobra accepted it (no
			// "unknown flag" complaint AND it did not fail on the flag), that is a
			// dispatch affordance.
			if flagAccepted(res) {
				return fail(c, "DISPATCH AFFORDANCE: `mad-substrate %s` accepted goal/dispatch flag %s: %s",
					strings.Join(sub, " "), flag, strings.TrimSpace(res.Out()))
			}
		}
	}

	// --- RPC: no dispatch-shaped method is registered. classify.route is a real
	// method we use only to confirm the connection is live (the method-probe baseline).
	cl, err := s.Dial()
	if err != nil {
		return fail(c, "dial: %v", err)
	}
	defer cl.Close()
	for _, m := range dispatchMethods {
		var out map[string]any
		err := cl.Call(m, map[string]any{}, &out)
		if err == nil {
			return fail(c, "DISPATCH METHOD: the registry served %q (a dispatch affordance exists)", m)
		}
		// The error must be a method-not-found, not an incidental param error on a
		// real-but-different method. method-not-found is the registry saying "no such
		// affordance" — exactly what we want.
		if !isMethodNotFound(err) {
			return fail(c, "dispatch method %q returned a non-not-found error (it may be partially wired): %v", m, err)
		}
	}

	return pass(c, "no subcommand accepts %v; no dispatch method (%v) is registered (task-intake seam empty)",
		dispatchFlags, dispatchMethods)
}

func (c noDispatch) Control(s *Scratch) error {
	// Non-vacuity has two halves, mirroring the two probes:
	//
	// (1) the FLAG probe must be able to tell ACCEPTED from REJECTED. --socket is a
	//     flag the CLI genuinely accepts; if flagAccepted reports it as rejected, the
	//     Run-side "rejected unknown dispatch flags" verdict is meaningless.
	res := s.CLI("trunk", "list", "--socket", s.Socket)
	if !flagAccepted(res) {
		return fmt.Errorf("CONTROL VACUOUS: the flag-probe says --socket (a REAL accepted flag) was rejected — it cannot tell accept from reject, so the no-dispatch-flag verdict is hollow: %s", strings.TrimSpace(res.Out()))
	}

	// (2) the METHOD probe must be able to tell PRESENT from ABSENT. diag.health is a
	//     registered method; if calling it looks like method-not-found, the probe
	//     cannot distinguish a registered dispatch method from an absent one.
	cl, err := s.Dial()
	if err != nil {
		return fmt.Errorf("control dial: %w", err)
	}
	defer cl.Close()
	var h map[string]any
	if err := cl.Call("diag.health", map[string]any{}, &h); err != nil {
		return fmt.Errorf("CONTROL VACUOUS: a known-registered method (diag.health) failed (%v) — the method-probe cannot confirm a method is present, so 'dispatch method absent' is meaningless", err)
	}

	// (3) #12: a REGISTERED method that errors with an entity-absent "not found"
	// (CodeNotFound -32002, e.g. "no such integration") must NOT be classified as
	// method-not-found. This proves the taxonomy match is precise — a partially-wired
	// dispatch method that errored "X not found" could otherwise pass as absent.
	var st map[string]any
	nf := cl.Call("integrate.status", map[string]any{"id": "int-does-not-exist"}, &st)
	if nf == nil {
		return fmt.Errorf("CONTROL: integrate.status of a bogus id should error (entity not found) but succeeded — cannot exercise the misclassification guard")
	}
	if isMethodNotFound(nf) {
		return fmt.Errorf("CONTROL VACUOUS: a REGISTERED method's entity-absent error (%v) was misclassified as METHOD-not-found — the over-broad 'not found' match would hide a partially-wired dispatch method", nf)
	}
	return nil
}

// flagAccepted reports whether the CLI treated the flag as RECOGNIZED. cobra
// rejects an unknown flag with exit!=0 and an "unknown flag"/"unknown shorthand"
// message; a recognized flag does not produce that message (the command may still
// fail later for other reasons, e.g. a missing daemon — that is not flag rejection).
func flagAccepted(res CLIResult) bool {
	out := strings.ToLower(res.Out())
	if strings.Contains(out, "unknown flag") || strings.Contains(out, "unknown shorthand flag") {
		return false
	}
	return true
}

// isMethodNotFound reports whether an rpc error is PRECISELY the registry's
// method-not-found (the public taxonomy code -32601 for "no such method"). The
// rpcclient formats an error as "rpc <method>: <message> (code <n>)".
//
// #12: it matches ONLY the precise taxonomy — the -32601 code OR the exact
// method-not-found phrasings. It deliberately does NOT match the over-broad bare
// substring "not found": a registered method erroring "session not found" /
// "integration not found" is a CodeNotFound (-32002) entity-absent error, NOT a
// method-absent one, and must never be misclassified as the method being absent
// (which would let a partially-wired dispatch method pass as "no such affordance").
func isMethodNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "-32601") ||
		strings.Contains(msg, "method not found") ||
		strings.Contains(msg, "no such method") ||
		strings.Contains(msg, "unknown method")
}
