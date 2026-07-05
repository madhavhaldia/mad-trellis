package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-trellis/internal/rpcclient"
	"github.com/madhavhaldia/mad-trellis/internal/runtimecfg"
)

func integrateCmd() *cobra.Command {
	var socket string
	var from string
	cmd := &cobra.Command{
		Use:   "integrate <branch>",
		Short: "Merge a spawned branch into the current branch, serialized by the trunk lease",
		Long: "integrate acquires the single trunk lease from the daemon (so two integrations never " +
			"write the trunk at once — Inv 6), merges <branch> into the current branch, then releases the " +
			"lease. A merge conflict aborts cleanly and reports it.\n\n" +
			"For a CONTAINER-grain boundary the agent's commits live only in its self-contained clone " +
			"(not this repo's object store), so pass --from <clone-path> (the HostWorktree path `spawn` " +
			"printed) to fetch the branch from the clone before merging.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			branch := args[0]
			socket = runtimecfg.SocketPath(socket)
			cl, err := rpcclient.Dial(socket)
			if err != nil {
				return fmt.Errorf("daemon not reachable (%w) — start `mad-trellis daemon`", err)
			}
			defer cl.Close()

			// The trunk lease key comes from the classifier (Route(trunk)).
			var route struct {
				LeaseKey string `json:"lease_key"`
			}
			if err := cl.Call("classify.route", map[string]string{"domain": "trunk"}, &route); err != nil {
				return err
			}
			if route.LeaseKey == "" {
				return fmt.Errorf("daemon returned no trunk lease key")
			}

			// Acquire the trunk lease; retry briefly if another integration holds it.
			acquired := false
			for i := 0; i < 120; i++ {
				var acq struct {
					Granted bool   `json:"granted"`
					Holder  string `json:"holder"`
				}
				if err := cl.Call("lease.acquire", map[string]any{"key": route.LeaseKey, "ttl_ms": 120000}, &acq); err != nil {
					return err
				}
				if acq.Granted {
					acquired = true
					break
				}
				if i == 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "trunk busy (held by %s) — waiting...\n", acq.Holder)
				}
				time.Sleep(time.Second)
			}
			if !acquired {
				return fmt.Errorf("could not acquire the trunk lease (still busy)")
			}
			defer cl.Call("lease.release", map[string]any{"key": route.LeaseKey}, nil)

			wd, _ := os.Getwd()
			if err := integrateMerge(wd, from, branch); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "integrated %s into %s\n", branch, currentBranch(wd))
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", socketFlagHelp)
	cmd.Flags().StringVar(&from, "from", "", "fetch <branch> from this clone path before merging (a container-grain boundary's self-contained clone — the HostWorktree path `spawn` printed)")
	return cmd
}

// integrateMerge performs the working-tree side of `integrate`, conducting git
// only (the trunk-lease serialization is the caller's — RunE holds the lease around
// this). When from != "", it first FETCHES <branch> from the standalone clone at
// `from` into the repo at `wd` (the CONTAINER-grain path: the agent's commits live
// only in its clone, not the canonical object store), then merges <branch> into
// wd's current branch with --no-ff; a conflict is aborted cleanly so the trunk is
// left byte-identical. Extracted from RunE so the fetch+merge+abort logic is
// unit-testable without a daemon.
func integrateMerge(wd, from, branch string) error {
	// Validate the dynamic args so neither can be parsed by git as a FLAG
	// (arg-injection): the branch is passed positionally to `git merge`, and `from`
	// positionally to `git fetch` as the remote. A leading '-' on either would be a
	// flag (e.g. `git merge -D`, `git fetch --upload-pack=...`).
	if !isSafeBranchName(branch) {
		return fmt.Errorf("unsafe branch name %q (must be non-empty, not start with '-', and match [A-Za-z0-9._/-])", branch)
	}
	if from != "" {
		if strings.HasPrefix(from, "-") {
			return fmt.Errorf("--from must not start with '-' (got %q)", from)
		}
		// It must be a real clone directory (this also rejects a flag-shaped value).
		if info, err := os.Stat(from); err != nil || !info.IsDir() {
			return fmt.Errorf("--from must be an existing clone directory (got %q): %v", from, err)
		}
		// Bring the clone's branch + objects into the canonical repo. +<b>:<b>
		// force-updates the local ref (idempotent with the grain's harvest-on-teardown,
		// which may have already fetched the same ref).
		if out, err := runGit(wd, "fetch", "--no-tags", from, "+"+branch+":"+branch); err != nil {
			return fmt.Errorf("fetch %s from %s: %w\n%s", branch, from, err, out)
		}
	}
	if out, err := runGit(wd, "merge", "--no-ff", "--no-edit", branch); err != nil {
		_, _ = runGit(wd, "merge", "--abort")
		return fmt.Errorf("conflict integrating %s (merge aborted):\n%s", branch, out)
	}
	return nil
}

// isSafeBranchName reports whether a branch/ref name is safe to pass to git as a
// POSITIONAL argument: non-empty, NOT starting with '-' (so git can never parse it
// as a flag), and only [A-Za-z0-9._/-] (covers nm/<slug>). This closes the
// arg-injection on the user-facing `integrate` command.
func isSafeBranchName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '/' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

func runGit(dir string, args ...string) (string, error) {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func currentBranch(dir string) string {
	out, _ := runGit(dir, "rev-parse", "--abbrev-ref", "HEAD")
	return out
}
