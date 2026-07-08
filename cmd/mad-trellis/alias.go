package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-trellis/internal/launcher"
)

// defaultAliasName is the short convenience name for the binary. `mt` = the
// initials of mad-trellis and reads cleanly (`mt launch -- claude`). (A
// historic `mt` magnetic-tape utility exists on some Linux distros; the
// symlink lives in the install BINDIR, which precedes /usr/bin on a typical
// PATH — and `make install ALIAS=` / `mad-trellis alias <name>` renames or
// disables it.)
const defaultAliasName = "mt"

// aliasCmd creates a short convenience alias for the binary so users don't type
// `mad-trellis` every time. It defaults to a SYMLINK next to the installed
// binary (shell-agnostic — works in scripts and every shell, unlike a shell
// `alias`); `--print` instead emits a shell-rc line for users who prefer that.
//
// The alias name must NOT be an agent shim name (claude/codex) — those argv[0]
// names are intercepted by the transparent launcher (Inv 13) and would NOT run
// the normal CLI — nor "mad-trellis" itself. `mt`/`sub`/etc. are not supported
// agents, so they fall straight through AgentFromArgv0 to the cobra CLI.
func aliasCmd() *cobra.Command {
	var dir string
	var printShell bool
	var force bool
	cmd := &cobra.Command{
		Use:   "alias [name]",
		Short: "Create a short convenience alias for the binary (default: mt)",
		Long: "Create a short convenience alias so you can type `mt` instead of `mad-trellis`.\n" +
			"By default this writes a symlink next to the installed binary (works in every\n" +
			"shell and in scripts). Use --print to emit a shell-rc `alias` line instead.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := defaultAliasName
			if len(args) == 1 {
				name = args[0]
			}
			if name == "" {
				return fmt.Errorf("alias name must not be empty")
			}
			// Reject names that collide with the shim dispatch or the binary itself:
			// a `claude`/`codex` argv[0] is routed through the governed launcher and
			// would never run the CLI, and `mad-trellis` is the binary's own name.
			if name == "mad-trellis" || launcher.IsSupportedAgent(name) {
				return fmt.Errorf("%q is reserved (agent shim names and \"mad-trellis\" cannot be aliases); pick e.g. mt or sub", name)
			}

			self, err := os.Executable()
			if err != nil {
				return fmt.Errorf("cannot resolve the running binary path: %w", err)
			}
			if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
				self = resolved
			}

			if printShell {
				// Quote the path in case it contains spaces; single quotes are safe
				// for a literal absolute path on a POSIX shell.
				fmt.Fprintf(cmd.OutOrStdout(), "alias %s='%s'\n", name, self)
				return nil
			}

			if dir == "" {
				dir = filepath.Dir(self)
			}
			target := filepath.Join(dir, name)

			// Idempotent: replace an existing alias symlink so it can't go stale.
			// Refuse to clobber a real file (something else owns that name) unless
			// --force, so we never delete an unrelated binary on PATH.
			if fi, lerr := os.Lstat(target); lerr == nil {
				if fi.Mode()&os.ModeSymlink == 0 && !force {
					return fmt.Errorf("%s already exists and is not a symlink — refusing to overwrite (use --force to replace)", target)
				}
				if rerr := os.Remove(target); rerr != nil {
					return fmt.Errorf("replace existing %s: %w", target, rerr)
				}
			}
			if serr := os.Symlink(self, target); serr != nil {
				return fmt.Errorf("create alias %s: %w", target, serr)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "alias installed: %s -> %s\n", target, self)
			if d := filepath.Dir(target); !dirOnPath(d) {
				fmt.Fprintf(cmd.OutOrStdout(), "NOTE: %s is not on your PATH — add it or run `mad-trellis alias --dir <a-PATH-dir>`\n", d)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to place the alias in (default: alongside the mad-trellis binary)")
	cmd.Flags().BoolVar(&printShell, "print", false, "print a shell-rc `alias` line instead of writing a symlink")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing non-symlink file at the alias path")
	return cmd
}

// dirOnPath reports whether dir is an entry in $PATH (best-effort, for an
// advisory note only).
func dirOnPath(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == "" {
			continue
		}
		if pa, perr := filepath.Abs(p); perr == nil {
			if pa == abs {
				return true
			}
		} else if p == dir {
			return true
		}
	}
	return false
}
