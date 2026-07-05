package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-trellis/internal/conformance"
)

// conformCmd is the CLI surface for project 10a (conformance-harness): the
// executable safety authority and the SOLE definer of self-hosting day. It runs
// the full AND-not-OR safety gate against THIS binary (os.Executable()) on a
// hermetic scratch runtime — its own daemon, its own trunk repo, never touching
// ~/.mad-trellis — prints the coverage matrix + per-check PASS/FAIL + a final
// GREEN/RED, and exits 0 ONLY if every safety clause held (non-zero on any fail).
// One authoritative green/red = the self-hosting-day signal.
func conformCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "conform",
		Short: "Run the safety-property conformance gate (the self-hosting-day acceptance gate)",
		Long: "conform boots a REAL governed scenario through the PUBLIC daemon contract + CLI ONLY and " +
			"exits non-zero on ANY safety-clause failure. It asserts the conjunction (a) forkable isolation + " +
			"no coordination channel, (b) no convergent write without an exclusive lease AND validated " +
			"integration, (c) no singular effect without a grant — plus the two-agent/one-lease/mediated E2E " +
			"acceptance gate. The harness is hermetic: its own daemon on a scratch runtime, never ~/.mad-trellis.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			self, err := os.Executable()
			if err != nil {
				return fmt.Errorf("conform: locate this binary: %w", err)
			}
			rep, err := conformance.RunGate(self)
			if err != nil {
				return err
			}
			rep.Print(cmd.OutOrStdout())
			if !rep.AllPass {
				// Non-zero exit on any failure — the authoritative RED signal.
				os.Exit(1)
			}
			return nil
		},
	}
}
