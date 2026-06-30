package launcher

import "testing"

// TestConvergeDecision is the exhaustive truth table for the PURE R10 (L1) converge-
// mode predicate. It pins each cell of {mode × isTTY × conductorOff} so the thin
// prompt I/O around it can stay trivial. The non-vacuous controls are called out
// inline: the prompt+noTTY row proves a missing terminal NEVER blocks convergence
// (it falls back to auto), and the conductorOff rows prove the legacy opt-out wins
// over ANY mode ("if either says off, it's off").
func TestConvergeDecision(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		isTTY    bool
		condOff  bool
		converge bool
		ask      bool
		off      bool
	}{
		// auto (default) ⇒ converge, regardless of TTY. This is the L0 byte-identical path.
		{"auto+tty", "auto", true, false, true, false, false},
		{"auto+notty", "auto", false, false, true, false, false},
		// unset / empty / unrecognized all normalize to auto.
		{"empty(unset)", "", true, false, true, false, false},
		{"unrecognized", "garble", false, false, true, false, false},
		// prompt ⇒ ASK only on an interactive TTY.
		{"prompt+tty asks", "prompt", true, false, false, true, false},
		// CONTROL: prompt with NO TTY must fall back to converge — a missing terminal
		// must never block (or hang) auto-convergence.
		{"prompt+notty falls back to converge", "prompt", false, false, true, false, false},
		// off ⇒ never converge; surface the staged hint.
		{"off+tty", "off", true, false, false, false, true},
		{"off+notty", "off", false, false, false, false, true},
		// CONTROL: the legacy MAD_CONDUCTOR opt-out forces off regardless of mode
		// or TTY — composition is "if EITHER says off, it's off".
		{"conductorOff over auto", "auto", true, true, false, false, true},
		{"conductorOff over prompt+tty", "prompt", true, true, false, false, true},
		{"conductorOff over prompt+notty", "prompt", false, true, false, false, true},
		{"conductorOff over off", "off", false, true, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := convergeDecision(tc.mode, tc.isTTY, tc.condOff)
			if got.converge != tc.converge || got.ask != tc.ask || got.off != tc.off {
				t.Fatalf("convergeDecision(%q, tty=%v, off=%v) = %+v; want {converge:%v ask:%v off:%v}",
					tc.mode, tc.isTTY, tc.condOff, got, tc.converge, tc.ask, tc.off)
			}
			// Structural invariant: EXACTLY one outcome is set — the call site switches
			// on these as mutually exclusive states.
			n := 0
			for _, b := range []bool{got.converge, got.ask, got.off} {
				if b {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("convergeDecision(%q, %v, %v) set %d outcomes, want exactly 1 (%+v)",
					tc.mode, tc.isTTY, tc.condOff, n, got)
			}
		})
	}
}
