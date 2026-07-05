package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-trellis/internal/mcp"
)

// mcpCmd runs the cooperative MCP server over stdio — the agent-facing tool
// surface (mad_locks/classify/claim/release/status) a
// launched agent calls to coordinate. It is a SECOND client of the frozen daemon
// registry and adds NO daemon methods. One server process == one connection ==
// one stable daemon-minted identity (Inv 4). FAIL-SOFT (Inv 13): it serves even
// with the daemon down (tools return a benign "proceeding is safe" note) and
// never fails an agent closed. stdout is the JSON-RPC channel and MUST stay clean
// — diagnostics go to stderr. `mad-trellis launch` wires this automatically; it is
// also runnable standalone in any MCP-capable host.
func mcpCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:          "mcp",
		Short:        "Run the cooperative MCP server over stdio (the mad_* tools)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A termination signal or stdin EOF ends the server. The parent agent
			// closes our stdin when it exits, which is the normal shutdown path;
			// SIGINT/SIGTERM cover a direct kill; SIGHUP covers a terminal-window
			// close/hangup (osascript Terminal.app SIGHUPs the process group). All
			// three route through the SAME clean-shutdown path so an integrator's
			// presence lease is RELEASED on exit rather than stranded until its TTL
			// lapses (Inv 3: no lock outlives its holder).
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			logf := func(format string, args ...any) {
				fmt.Fprintf(os.Stderr, "mad-trellis mcp: "+format+"\n", args...)
			}
			return mcp.Serve(ctx, os.Stdin, os.Stdout, version, role, logf)
		},
	}
	// --role selects the toolset: builder (default) exposes the cooperative tools
	// plus request_integration/integration_status; integrator exposes the
	// trunk-side pending/claim/approve/reject reviewer tools. An unknown value
	// falls back to builder.
	cmd.Flags().StringVar(&role, "role", "builder", "MCP toolset role: builder|integrator")
	return cmd
}
