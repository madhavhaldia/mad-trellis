package main

import (
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/madhavhaldia/mad-substrate/internal/launcher"
)

// TestIntegratorPromptDisabled pins the opt-out predicate: off/0/false/no
// (any case, trimmed) disable the launch-time prompt; everything else enables it.
func TestIntegratorPromptDisabled(t *testing.T) {
	disabled := []string{"off", "0", "false", "no", "OFF", " no ", "False"}
	for _, v := range disabled {
		if !integratorPromptDisabled(v) {
			t.Errorf("integratorPromptDisabled(%q) = false, want true", v)
		}
	}
	enabled := []string{"", "on", "1", "true", "yes", "garbage", "offish"}
	for _, v := range enabled {
		if integratorPromptDisabled(v) {
			t.Errorf("integratorPromptDisabled(%q) = true, want false", v)
		}
	}
}

// TestMaybePromptIntegratorNonTTY: with stdin NOT a char device (a pipe, as under
// `go test`), maybePromptIntegrator is a no-op — it never prompts, never reads
// stdin, never blocks, and never panics. This is the headless/piped guarantee.
func TestMaybePromptIntegratorNonTTY(t *testing.T) {
	// Force a non-TTY stdin (an os.Pipe read end is not a ModeCharDevice).
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig }()

	// Prompt enabled, bogus socket: must still be a silent no-op on non-TTY stdin
	// (the TTY gate short-circuits before any daemon dial or stdin read).
	t.Setenv("MAD_INTEGRATOR_PROMPT", "on")
	maybePromptIntegrator("/nonexistent.sock", "claude")
}

// flagNames returns every flag name registered on cmd (local + persistent).
func flagNames(cmd *cobra.Command) []string {
	var names []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) { names = append(names, f.Name) })
	cmd.PersistentFlags().VisitAll(func(f *pflag.Flag) { names = append(names, f.Name) })
	return names
}

// The actual `mad-substrate launch` surface must expose NO goal/dispatch affordance
// (Inv 13 no-goals/no-dispatch). The agent's own args after `--` are opaque
// pass-through and are NOT part of mad-substrate's surface.
func TestLaunchCommandHasNoGoalAffordance(t *testing.T) {
	off := launcher.AuditNoGoals(flagNames(launchCmd()))
	if len(off) != 0 {
		t.Fatalf("the launch command exposes goal/dispatch affordance(s): %v", off)
	}
}

// POSITIVE CONTROL: a launch command that DID grow a --goal flag must be caught.
func TestLaunchCommandNoGoalPositiveControl(t *testing.T) {
	c := launchCmd()
	var goal string
	c.Flags().StringVar(&goal, "goal", "", "the forbidden affordance")
	if len(launcher.AuditNoGoals(flagNames(c))) == 0 {
		t.Fatal("REGRESSION: a launch command with a --goal flag was not flagged")
	}
}
