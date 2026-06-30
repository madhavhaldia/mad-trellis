package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
	"github.com/madhavhaldia/mad-substrate/internal/runtimecfg"
)

// trunkCmd is the CLI surface for project 6 (integrator-trunk): the mediated,
// validated, lease-gated trunk integrator. It is distinct from the bootstrap
// `integrate` (a plain working-tree git-merge): `trunk` drives the real
// subsystem — submit a pushed branch, then promote it onto the mediated trunk
// through the daemon (the integrator is the sole promoter). Agents reach the
// integrator only through these daemon methods.
func trunkCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:   "trunk",
		Short: "Mediated trunk integrator (submit/promote/abort/status/list)",
		Long: "trunk drives project 6, the single trunk integrator. A branch is SUBMITTED, then " +
			"PROMOTED: the integrator validates it merges cleanly (the gate), acquires the trunk lease " +
			"(single-writer), and advances the mediated trunk via one atomic update-ref. promote/abort " +
			"are idempotent; a mid-integration death leaves trunk clean by construction.",
	}
	cmd.PersistentFlags().StringVar(&socket, "socket", "", socketFlagHelp)
	dial := func() (*rpcclient.Client, error) {
		s := runtimecfg.SocketPath(socket)
		cl, err := rpcclient.Dial(s)
		if err != nil {
			return nil, fmt.Errorf("daemon not reachable (%w) — start `mad-substrate daemon`", err)
		}
		return cl, nil
	}

	submit := &cobra.Command{
		Use:   "submit <branch-ref>",
		Short: "Submit a pushed branch for integration (returns an integration id)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := dial()
			if err != nil {
				return err
			}
			defer cl.Close()
			var out struct {
				ID    string `json:"id"`
				State string `json:"state"`
				Base  string `json:"base"`
			}
			if err := cl.Call("integrate.submit", map[string]string{"branch": args[0]}, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "submitted %s (%s, base %s)\n", out.ID, out.State, short12(out.Base))
			return nil
		},
	}

	promote := &cobra.Command{
		Use:   "promote <id>",
		Short: "Validate + atomically promote an integration onto trunk (idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOutcome(cmd, dial, "integrate.promote", args[0])
		},
	}
	abort := &cobra.Command{
		Use:   "abort <id>",
		Short: "Abort an in-flight integration (idempotent; trunk untouched)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOutcome(cmd, dial, "integrate.abort", args[0])
		},
	}
	status := &cobra.Command{
		Use:   "status <id>",
		Short: "Show an integration's reconciled state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOutcome(cmd, dial, "integrate.status", args[0])
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List all integrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := dial()
			if err != nil {
				return err
			}
			defer cl.Close()
			var out struct {
				Integrations []struct {
					ID          string `json:"id"`
					Branch      string `json:"branch"`
					Holder      string `json:"holder"`
					State       string `json:"state"`
					Base        string `json:"base"`
					MergeCommit string `json:"merge_commit"`
				} `json:"integrations"`
			}
			if err := cl.Call("integrate.list", nil, &out); err != nil {
				return err
			}
			if len(out.Integrations) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no integrations)")
				return nil
			}
			for _, r := range out.Integrations {
				fmt.Fprintf(cmd.OutOrStdout(), "%s  %-10s  %-28s  base=%s merge=%s\n",
					r.ID, r.State, r.Branch, short12(r.Base), short12(r.MergeCommit))
			}
			return nil
		},
	}

	cmd.AddCommand(submit, promote, abort, status, list)
	return cmd
}

func runOutcome(cmd *cobra.Command, dial func() (*rpcclient.Client, error), method, id string) error {
	cl, err := dial()
	if err != nil {
		return err
	}
	defer cl.Close()
	var out struct {
		ID        string `json:"id"`
		State     string `json:"state"`
		Promoted  bool   `json:"promoted"`
		TrunkTip  string `json:"trunk_tip"`
		Reason    string `json:"reason"`
		Retryable bool   `json:"retryable"`
	}
	if err := cl.Call(method, map[string]string{"id": id}, &out); err != nil {
		return err
	}
	msg := fmt.Sprintf("%s: %s", out.ID, out.State)
	if out.Promoted {
		msg += fmt.Sprintf(" (trunk now %s)", short12(out.TrunkTip))
	}
	if out.Reason != "" {
		msg += " — " + out.Reason
	}
	if out.Retryable {
		msg += " [retryable]"
	}
	fmt.Fprintln(cmd.OutOrStdout(), msg)
	return nil
}

func short12(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	if oid == "" {
		return "∅"
	}
	return oid
}
