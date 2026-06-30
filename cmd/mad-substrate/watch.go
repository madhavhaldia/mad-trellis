package main

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-substrate/internal/runtimecfg"
	"github.com/madhavhaldia/mad-substrate/internal/watch"
)

// watchCmd is the CLI surface for project 9a (watch-view-surface): the
// host-agnostic, READ-ONLY "seventh terminal" that mirrors the live governance
// loop. It is NON-load-bearing by construction — it dials the daemon through a
// read-only client (no mutating RPC is reachable) and polls a fresh snapshot on
// an interval. Killing it or never starting it changes ZERO governed outcomes,
// so a dead socket is a friendly message and a clean exit, never an error path
// that could be mistaken for a governance fault.
func watchCmd() *cobra.Command {
	var (
		socket   string
		interval time.Duration
	)
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Read-only TUI mirroring the live governance loop (trunk, integrations, leases, audit)",
		Long: "watch is the seventh terminal: a read-only view of the combined trunk/integration/lease/" +
			"decision-audit state, observed in place. It can call ONLY read RPC methods — there is no " +
			"affordance to approve, retry, abort, promote, or dispatch anything. It is non-load-bearing: " +
			"killing it never changes a governed outcome.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := runtimecfg.SocketPath(socket)
			cl, err := watch.Dial(s, 0)
			if err != nil {
				// Non-load-bearing: a dead socket is informational, not a fault.
				fmt.Fprintf(cmd.OutOrStdout(),
					"cannot reach daemon at %s — start `mad-substrate daemon`.\n(the watch view is read-only and non-load-bearing; nothing is wrong with governance)\n",
					s)
				return nil
			}
			defer cl.Close()

			fetch := watch.NewClientFetcher(cl, watch.DefaultAuditLimit)
			model := watch.NewModel(fetch, interval)
			prog := tea.NewProgram(model, tea.WithAltScreen())
			_, err = prog.Run()
			return err
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", socketFlagHelp)
	cmd.Flags().DurationVar(&interval, "interval", watch.DefaultInterval, "poll interval")
	return cmd
}
