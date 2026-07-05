package main

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-trellis/internal/rpcclient"
	"github.com/madhavhaldia/mad-trellis/internal/runtimecfg"
)

// gateCmd is the CLI surface for project 7 (singular-gate): the default-deny
// boundary for resources with real external side effects. `resolve` shows the
// grant decision (deny/mock/proxy/supervised); `request` produces the singular
// env-spec portion (acquiring the serialized lease for a supervised grant);
// `release` frees a held supervised grant. DENY is the structural ground state —
// an undeclared/ungranted resource routes to no reachable real endpoint.
func gateCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:   "gate",
		Short: "Singular-resource default-deny gate (resolve/request/release)",
	}
	cmd.PersistentFlags().StringVar(&socket, "socket", "", socketFlagHelp)
	dial := func() (*rpcclient.Client, error) {
		s := runtimecfg.SocketPath(socket)
		cl, err := rpcclient.Dial(s)
		if err != nil {
			return nil, fmt.Errorf("daemon not reachable (%w) — start `mad-trellis daemon`", err)
		}
		return cl, nil
	}

	resolve := &cobra.Command{
		Use:   "resolve <resource>",
		Short: "Show the grant decision for a resource (no side effect)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := dial()
			if err != nil {
				return err
			}
			defer cl.Close()
			var out struct {
				Resource string `json:"resource"`
				Singular bool   `json:"singular"`
				Mode     string `json:"mode"`
				Reason   string `json:"reason"`
			}
			if err := cl.Call("singular.resolve", map[string]string{"resource": args[0]}, &out); err != nil {
				return err
			}
			line := fmt.Sprintf("%s: %s", out.Resource, out.Mode)
			if !out.Singular {
				line += " (not singular — gate not responsible)"
			}
			if out.Reason != "" {
				line += " — " + out.Reason
			}
			fmt.Fprintln(cmd.OutOrStdout(), line)
			return nil
		},
	}

	request := &cobra.Command{
		Use:   "request <resource>",
		Short: "Produce the singular env-spec for a resource (acquires a supervised grant if granted)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := dial()
			if err != nil {
				return err
			}
			defer cl.Close()
			var out struct {
				Resource      string            `json:"resource"`
				Mode          string            `json:"mode"`
				Granted       bool              `json:"granted"`
				RealReachable bool              `json:"real_reachable"`
				Env           map[string]string `json:"env"`
				Reason        string            `json:"reason"`
			}
			if err := cl.Call("singular.request", map[string]string{"resource": args[0]}, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: mode=%s granted=%v real_reachable=%v\n",
				out.Resource, out.Mode, out.Granted, out.RealReachable)
			keys := make([]string, 0, len(out.Env))
			for k := range out.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s=%s\n", k, out.Env[k])
			}
			if out.Reason != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  (%s)\n", out.Reason)
			}
			return nil
		},
	}

	release := &cobra.Command{
		Use:   "release <resource>",
		Short: "Release a held supervised grant",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := dial()
			if err != nil {
				return err
			}
			defer cl.Close()
			var out struct {
				OK bool `json:"ok"`
			}
			if err := cl.Call("singular.release", map[string]string{"resource": args[0]}, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "released %s: %v\n", args[0], out.OK)
			return nil
		},
	}

	cmd.AddCommand(resolve, request, release)
	return cmd
}
