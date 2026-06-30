package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-substrate/internal/coophook"
)

// hookCmd runs a single cooperative session-guidance hook event for a launched
// agent's CLI host (Claude Code). It is STATELESS and ADVISORY — the only event
// it serves, claude-sessionstart, injects standing guidance that tells the agent
// how to coordinate via the mad-substrate MCP tools; it holds no lease and makes no
// per-edit decision. FAIL-SOFT (Inv 13): every event exits 0 — so a hook can
// never make a governed session more fragile than a bare one. The event names are
// an internal contract with `mad-substrate launch`'s wiring, so the command is hidden
// from the user-facing help.
func hookCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "hook <event>",
		Short:         "Run a cooperative session-guidance hook event (wired by launch)",
		Args:          cobra.ArbitraryArgs,
		Hidden:        true,
		SilenceUsage:  true,
		SilenceErrors: true,
		Run: func(cmd *cobra.Command, args []string) {
			// FAIL-SOFT (Inv 13): the hook must NEVER exit non-zero, so we do NOT let
			// cobra's arg validation gate it (a failed validator would make the root
			// print an error and exit 1). The first arg is the event; an empty or
			// unknown event maps to a silent allow inside Run. Extra args are ignored.
			event := ""
			if len(args) > 0 {
				event = args[0]
			}
			os.Exit(coophook.Run(event, os.Stdin, os.Stdout, os.Stderr))
		},
	}
}
