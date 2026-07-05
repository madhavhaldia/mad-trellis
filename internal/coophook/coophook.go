// Package coophook is the HOST-side cooperative hook surface — the native Go
// implementation of the SessionStart standing-guidance hook for Claude Code. The
// host invokes the hook out-of-process at session start; the hook injects the
// standing instruction that tells the agent HOW to coordinate via the mad-trellis
// MCP tools.
//
// There is NO per-edit "claim-before-edit" interception here: the cooperative
// layer's value path is the MCP tools (which an agent calls to coordinate) plus
// this standing guidance (which tells it to use them). This package holds no
// lease, reads no daemon state, and makes no per-resource decision.
//
// FAIL-SOFT IS THE LAW (Inv 13): a governed session must never be more fragile
// than a bare one. Every path here ALWAYS returns process exit code 0; an
// unknown event (or a panic) writes nothing and still exits 0, so the hook can
// never gate or crash the agent.
package coophook

import (
	"encoding/json"
	"io"
)

// Run dispatches one hook invocation. event selects the lifecycle point; out is
// the hook's stdout. ONLY "claude-sessionstart" emits anything (the standing
// guidance); every other event — including any removed per-edit event — writes
// nothing and returns 0. It ALWAYS returns 0: the soft layer never fails closed
// via the exit code (Inv 13), and a panic anywhere is recovered to 0 so the host
// process the hook fronts can never be crashed by us.
func Run(event string, _ io.Reader, out, _ io.Writer) (code int) {
	// Outermost guard: ANY panic (library bug, nil deref) must still exit 0 and
	// emit nothing rather than crash the agent's session.
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()

	switch event {
	case "claude-sessionstart":
		return runClaudeSessionStart(out)
	default:
		// Unknown/removed event: write nothing, return 0. We never assume a
		// schema we do not recognize.
		return 0
	}
}

// sessionStartGuidance is the standing instruction injected into a Claude Code
// session at start, via the SessionStart hook's additionalContext channel. It
// mirrors the MCP server's `instructions` field (the two reinforce each other:
// the server hints WHEN to reach for the tools, this tells the agent HOW to work
// within the boundary). It is purely informational — never a constraint.
const sessionStartGuidance = "You are working inside a mad-trellis governed boundary: an isolated git worktree with its own ports and state. Edits to your working tree are private and safe. For SHARED resources that must merge to the trunk (convergent) or real external side effects (singular), coordinate using the mad-trellis MCP tools: mad_classify (does this path need coordination?), mad_claim (take it so other agents see it as held). Forkable paths need no claim — edit freely. mad_status and mad_locks show current contention. Do NOT merge your own branch: just commit your work on your boundary branch — convergence to the trunk happens outside the session (via `mad-trellis integrate` / the lead). Safety is guaranteed by the substrate regardless; these tools just help you avoid wasted work from conflicting edits."

// runClaudeSessionStart injects the standing guidance into a new Claude Code
// session. It does NOT read stdin (the SessionStart payload carries nothing we
// need) and is purely additive — it emits only additionalContext, never a
// decision — so it can never gate a session. Fail-soft: a marshal failure writes
// nothing and still returns 0.
func runClaudeSessionStart(out io.Writer) int {
	b := mustCompactJSON(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": sessionStartGuidance,
		},
	})
	if b != nil {
		_, _ = out.Write(b)
	}
	return 0
}

// mustCompactJSON marshals v to compact JSON. Our inputs are plain
// map[string]any of strings, so marshal cannot fail; on the impossible error we
// return nil (emit nothing) to honor fail-soft rather than panic.
func mustCompactJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
