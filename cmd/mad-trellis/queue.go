package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-trellis/internal/rpcclient"
	"github.com/madhavhaldia/mad-trellis/internal/runtimecfg"
)

// queueCmd is the read-only window onto same-path contention (Wing 4, R8): it
// resolves a resource to its convergent lease key (via the classifier, the ONLY
// legitimate source of a key — never fabricated) and prints who HOLDS the key plus
// the ORDERED intent queue behind it. It mutates nothing — it is the polling
// surface a waiter uses to discover its turn (the queue is headless; there is no
// push channel). `<resource>` is `trunk` for the global merge lease, or a path for
// a per-path convergent key.
func queueCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:   "queue <resource>",
		Short: "Show the holder and ordered intent queue for a convergent resource (Wing 4)",
		Long: "queue prints the current holder and the FIFO intent queue for a resource's convergent " +
			"lease key, resolved through the classifier (Route). `<resource>` is `trunk` (the global merge " +
			"lease) or a repo path (a per-path convergent key). It is READ-ONLY — the surface a waiter polls " +
			"to discover its turn after `lease.enqueue` (the queue is headless: no push, the head-of-queue " +
			"waiter wins the next free acquire).",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			resource := args[0]
			socket = runtimecfg.SocketPath(socket)
			cl, err := rpcclient.Dial(socket)
			if err != nil {
				return fmt.Errorf("daemon not reachable (%w) — start `mad-trellis daemon`", err)
			}
			defer cl.Close()

			// Resolve the lease key from the classifier — trunk is its own domain; any
			// other resource is routed as a path (per-path convergent key).
			domain, name := "path", resource
			if resource == "trunk" {
				domain, name = "trunk", ""
			}
			var route struct {
				Kind     string  `json:"kind"`
				LeaseKey *string `json:"lease_key"`
			}
			if err := cl.Call("classify.route", map[string]any{"domain": domain, "name": name}, &route); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if route.LeaseKey == nil {
				fmt.Fprintf(out, "%s is not a convergent (leased) resource (kind=%s) — nothing to queue on\n", resource, route.Kind)
				return nil
			}

			var snap struct {
				Held    bool   `json:"held"`
				Holder  string `json:"holder"`
				Waiters []struct {
					Session  string `json:"session"`
					Position int    `json:"position"`
				} `json:"waiters"`
			}
			if err := cl.Call("lease.queue", map[string]any{"key": *route.LeaseKey}, &snap); err != nil {
				return err
			}

			if snap.Held {
				fmt.Fprintf(out, "resource %s — held by %s\n", resource, snap.Holder)
			} else {
				fmt.Fprintf(out, "resource %s — free\n", resource)
			}
			if len(snap.Waiters) == 0 {
				fmt.Fprintln(out, "  queue: (empty)")
				return nil
			}
			fmt.Fprintf(out, "  queue (%d waiting):\n", len(snap.Waiters))
			for _, w := range snap.Waiters {
				fmt.Fprintf(out, "    %d. %s\n", w.Position, w.Session)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", socketFlagHelp)
	return cmd
}
