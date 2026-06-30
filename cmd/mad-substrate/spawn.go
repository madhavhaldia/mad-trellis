package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-substrate/internal/launcher"
	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
	"github.com/madhavhaldia/mad-substrate/internal/runtimecfg"
	"github.com/madhavhaldia/mad-substrate/internal/substrate"
)

func spawnCmd() *cobra.Command {
	var socket string
	var grain string
	cmd := &cobra.Command{
		Use:   "spawn [-- command ...]",
		Short: "Provision an isolated forkable boundary from the daemon and (optionally) run a command in it",
		Long: "spawn asks the daemon's isolation substrate to construct a per-agent boundary — a git " +
			"worktree off HEAD on a fresh nm/<session> branch, a disjoint port block, and private " +
			"scratch/cache/state dirs (Inv 1) — keyed off the daemon's unspoofable session identity. With " +
			"no command it prints where to work; with `-- <cmd>` it runs the command in the boundary on a " +
			"PTY, with the env-spec applied. Integrate the branch back with `mad-substrate integrate <branch>`. " +
			"(spawn is the fire-and-forget BOOTSTRAP — it does NOT hold the session or tear the boundary " +
			"down, leaking the daemon's reservation, chafe C6; the transparent launcher `mad-substrate launch` " +
			"is the governed, clean-exit path.)",
		RunE: func(cmd *cobra.Command, args []string) error {
			socket = runtimecfg.SocketPath(socket)
			// GRAIN DIAL (Inv 10): --grain exports MAD_GRAIN for parity with
			// `launch`. Unlike launch, spawn does NOT auto-start a daemon — it
			// provisions against whatever daemon is already running, whose grain was
			// fixed at its construction. So --grain here only takes effect for a daemon
			// that reads this env at start; the boundary's actual grain is whatever the
			// running daemon reports (spec.Grain), and the exec path follows that.
			if err := applyGrainSelection(grain); err != nil {
				return err
			}
			cli, err := rpcclient.Dial(socket)
			if err != nil {
				return fmt.Errorf("cannot reach the daemon (%w) — start it with `mad-substrate daemon`", err)
			}
			defer cli.Close()

			var spec substrate.Wire
			if err := cli.Call("substrate.provision", map[string]any{}, &spec); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"spawned %s boundary for session %s\n  cwd:    %s\n  branch: %s\n  ports:  %v\n",
				spec.Grain, spec.Session, spec.Cwd, spec.Branch, spec.Ports)
			if spec.ContainerID != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  container: %s\n", spec.ContainerID)
			}
			if len(args) == 0 {
				// The container grain's cwd is the IN-CONTAINER mount (/work), not a host
				// path to cd into; its commits live in the self-contained clone at
				// HostWorktree, integrated with `integrate --from <clone> <branch>`. The
				// worktree grain works directly in the host worktree (cd + plain integrate).
				if spec.Grain == "container" {
					// Single-quote the host clone path in the copy-paste hint so it
					// survives a home/worktree dir containing spaces.
					fmt.Fprintf(cmd.OutOrStdout(),
						"  # agent runs INSIDE the container at %s; host clone: %s\n"+
							"  # then: mad-substrate integrate --from '%s' %s\n",
						spec.Cwd, spec.HostWorktree, spec.HostWorktree, spec.Branch)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(),
						"  cd %s   # work here, then: mad-substrate integrate %s\n", spec.Cwd, spec.Branch)
				}
				return nil
			}
			// Reuse the launcher's hardened PTY plumbing (Inv 13). spawn stays
			// fire-and-forget; the governed clean-exit path is `mad-substrate launch`.
			// The exec target is wire-driven, so a container-grain boundary execs the
			// command INSIDE the container exactly as `launch` does.
			code, err := launcher.RunPTY(launcher.ExecTarget{
				Grain:       spec.Grain,
				Cwd:         spec.Cwd,
				ContainerID: spec.ContainerID,
			}, spec.Env, args[0], args[1:])
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", socketFlagHelp)
	cmd.Flags().StringVar(&grain, "grain", "", grainFlagHelp)
	return cmd
}
