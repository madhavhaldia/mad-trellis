package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
	"github.com/madhavhaldia/mad-substrate/internal/runtimecfg"
)

// recoverCmd triggers an on-demand liveness-recovery pass (project 8): reclaim
// expired leases, abort dead mid-integration holders, tear down their boundaries.
// The daemon also runs this periodically; this is the operator/test trigger. It
// only INVOKES the idempotent reclaim/abort triggers — it mutates nothing
// directly — so running it can never reclaim a still-live holder.
func recoverCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Run a liveness-recovery pass (reclaim expired leases, abort dead integrations)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			socket = runtimecfg.SocketPath(socket)
			cl, err := rpcclient.Dial(socket)
			if err != nil {
				return fmt.Errorf("daemon not reachable (%w) — start `mad-substrate daemon`", err)
			}
			defer cl.Close()
			var out struct {
				Reclaimed   int      `json:"reclaimed"`
				Aborted     int      `json:"aborted"`
				TornDown    int      `json:"torn_down"`
				DeadHolders []string `json:"dead_holders"`
			}
			if err := cl.Call("liveness.scan", nil, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "recovery: reclaimed=%d aborted=%d torn_down=%d dead=%v\n",
				out.Reclaimed, out.Aborted, out.TornDown, out.DeadHolders)
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", socketFlagHelp)
	return cmd
}
