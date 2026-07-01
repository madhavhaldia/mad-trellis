// Command mad-substrate is the CLI entrypoint and headless daemon for the mad-substrate
// governance substrate. Besides the explicit subcommands, the binary doubles as
// the transparent agent SHIM (project 5): when invoked under a supported agent's
// name (argv[0] is `claude`/`codex`, not `mad-substrate`) it routes that invocation
// through the governed launcher instead of running its own CLI — the mechanism
// that makes governance ambient (Inv 13).
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-substrate/internal/app"
	"github.com/madhavhaldia/mad-substrate/internal/buildinfo"
	"github.com/madhavhaldia/mad-substrate/internal/launcher"
	"github.com/madhavhaldia/mad-substrate/internal/manifest"
	"github.com/madhavhaldia/mad-substrate/internal/protocol"
	"github.com/madhavhaldia/mad-substrate/internal/runtimecfg"
)

// Build metadata, overridden via -ldflags -X (project 10b owns release builds).
var (
	version = "0.0.0-dev"
	commit  = "unknown"
)

// socketFlagHelp is the shared --socket help string across every CLI surface,
// describing the resolution precedence enforced by internal/runtimecfg.
const socketFlagHelp = "daemon socket (default: $MAD_SOCKET, else <runtime-dir>/daemon.sock; runtime-dir=$MAD_RUNTIME_DIR|$MAD_HOME|~/.mad-substrate)"

// perRepoRuntimeRoot records the canonical per-repo identity (the resolved git
// COMMON dir — shared by a repo's main worktree and every linked worktree) when
// per-repo runtime auto-defaulting was applied this run (else ""), so `doctor` can
// report the runtime dir's origin.
var perRepoRuntimeRoot string

// applyPerRepoRuntimeDefault gives each git repo its OWN mad-substrate runtime
// (socket/ledger/trunk) by default, so `cd <repo> && mad-substrate …` never collides
// with another repo's daemon and needs no MAD_RUNTIME_DIR juggling. It acts
// ONLY when the user expressed no runtime preference (none of MAD_RUNTIME_DIR
// / MAD_HOME / MAD_SOCKET set) AND cwd is inside a git repo; it then
// EXPORTS MAD_RUNTIME_DIR = the per-repo dir so every downstream surface — this
// CLI, the auto-started daemon (inherits the env), and a launched agent's adapter
// (inherits it, so it uses the launcher's resolution rather than mis-deriving from
// its boundary cwd) — agrees on one per-repo socket. Outside a repo (or with any
// override set) it is a no-op and the global ~/.mad-substrate default stands. Called
// once at the very top of main, before the shim dispatch and any socket resolution.
func applyPerRepoRuntimeDefault() {
	if os.Getenv("MAD_RUNTIME_DIR") != "" || os.Getenv("MAD_HOME") != "" || os.Getenv("MAD_SOCKET") != "" {
		return // the user pinned a runtime/home/socket — respect it
	}
	id := cwdRepoIdentity()
	if id == "" {
		return // not in a git repo — keep the global ~/.mad-substrate default
	}
	dir := runtimecfg.PerRepoRuntimeDir(id)
	if dir == "" {
		return
	}
	_ = os.Setenv("MAD_RUNTIME_DIR", dir)
	perRepoRuntimeRoot = id
}

// cwdRepoIdentity returns the canonical per-repo identity of the git repo
// containing cwd: its SHARED git database directory (`git rev-parse
// --git-common-dir`), made absolute and symlink-resolved. The common dir is
// IDENTICAL for a repo's main worktree and every linked `git worktree`, so two
// checkouts of one repo (e.g. a main checkout and a Supacode-managed worktree)
// resolve to ONE runtime/daemon/ledger/trunk instead of mis-deriving separate ones
// from their distinct working paths. Returns "" when cwd is not in a git repo (or
// git is unavailable / errors) so the caller falls back to the global ~/.mad-substrate
// default. Symlink-resolved so /var vs /private/var can't produce two runtimes.
// cwdInGitRepo reports whether cwd is inside a git repository — the precondition
// for provisioning ANY boundary (every grain uses a git worktree/clone; the trunk
// is a git ref). It reuses cwdRepoIdentity's detection so the launch precheck and
// the per-repo runtime resolver agree on what "in a repo" means.
func cwdInGitRepo() bool { return cwdRepoIdentity() != "" }

func cwdRepoIdentity() string {
	out, err := exec.Command("git", "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return ""
	}
	// --git-common-dir may be relative to cwd (commonly the bare ".git"); anchor it.
	if !filepath.IsAbs(p) {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			p = abs
		}
	}
	if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
		return resolved
	}
	return p
}

func main() {
	// PER-REPO RUNTIME (default): give this repo its own socket/ledger/trunk so the
	// command works in any project with no env juggling. No-op under an explicit
	// override or outside a repo. MUST run before the shim dispatch + any resolution.
	applyPerRepoRuntimeDefault()

	// SHIM DISPATCH (fail-closed, Inv 4): if the binary was invoked through a
	// shim — argv[0] is a supported agent name, not "mad-substrate" — route the WHOLE
	// invocation through the governed launcher and never reach the normal CLI.
	// governedLaunch resolves the real agent fail-closed and os.Exit()s with the
	// child's code, so it does not return.
	if agent := launcher.AgentFromArgv0(os.Args[0]); agent != "" {
		governedLaunch(agent, os.Args[1:], runtimecfg.SocketPath(""), 0)
		return // unreachable: governedLaunch exits
	}

	root := &cobra.Command{
		Use:           "mad-substrate",
		Short:         "mad-substrate — a governance substrate for parallel agentic development",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(daemonCmd(), versionCmd(), doctorCmd(), initCmd(), aliasCmd(), spawnCmd(), despawnCmd(), integrateCmd(), integratorCmd(), trunkCmd(), gateCmd(), recoverCmd(), launchCmd(), shimCmd(), watchCmd(), conformCmd(), mcpCmd(), hookCmd(), queueCmd())
	if err := root.Execute(); err != nil {
		// A command may request a specific exit code without a noisy diagnostic
		// (e.g. `daemon status` printing "not running" then exiting 1).
		if se, ok := err.(errSilentExit); ok {
			os.Exit(se.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// defaultRuntimeDir is the mad-substrate per-user runtime directory (socket, ledger,
// shims). It now delegates to internal/runtimecfg, the single resolver shared by
// every Go surface (precedence: MAD_RUNTIME_DIR, then MAD_HOME, then
// ~/.mad-substrate); the dir is ensured (MkdirAll 0700).
func defaultRuntimeDir() string {
	return runtimecfg.RuntimeDir()
}

func daemonCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the headless arbiter daemon (single instance per socket)",
		// Bare `mad-substrate daemon` (no subcommand) runs this RunE and starts the
		// daemon; `daemon stop`/`daemon status` are control subcommands that
		// inherit the persistent --socket flag below. (cobra runs the parent RunE
		// only when no subcommand matches the args.)
		RunE: func(cmd *cobra.Command, _ []string) error {
			socket = runtimecfg.SocketPath(socket) // flag wins; empty -> env/default
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			ledger := filepath.Join(filepath.Dir(socket), "ledger.db")
			d, closeLedger, err := app.Build(app.Config{
				SocketPath: socket,
				LedgerPath: ledger,
				RepoRoot:   wd,
			})
			if err != nil {
				return err
			}
			defer closeLedger()
			if err := d.Start(); err != nil {
				return err // ErrAlreadyRunning when a live daemon already owns the socket
			}
			defer d.Close()

			// Graceful shutdown: a signal closes the listener, unblocking Serve.
			sigc := make(chan os.Signal, 1)
			signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-sigc
				_ = d.Close()
			}()

			// Scratch/test daemons (booted by `conform`) opt into dying with their parent so
			// an interrupted run never strands a daemon. The PRODUCTION daemon is meant to be
			// detached and never sets this, so its lifetime is unchanged.
			if exitWithParent() {
				startPPID := os.Getppid()
				go func() {
					t := time.NewTicker(time.Second)
					defer t.Stop()
					for range t.C {
						// Parent died → we were reparented (to init/launchd, pid 1) → shut down.
						if os.Getppid() != startPPID {
							_ = d.Close()
							os.Exit(0)
						}
					}
				}()
			}

			fmt.Fprintf(cmd.OutOrStdout(), "mad-substrate daemon listening on %s (ledger %s)\n", socket, ledger)
			return d.Serve()
		},
	}
	// PersistentFlag so the stop/status subcommands inherit --socket.
	cmd.PersistentFlags().StringVar(&socket, "socket", "", socketFlagHelp)
	cmd.AddCommand(daemonStatusCmd(&socket), daemonStopCmd(&socket))
	return cmd
}

// exitWithParent reports whether this daemon should self-terminate when its
// parent process dies (opt-in for scratch/test daemons via MAD_EXIT_WITH_PARENT).
func exitWithParent() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MAD_EXIT_WITH_PARENT"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Scaffold the mad-substrate manifest in the current repo (writes only mad-substrate.json)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			created, err := manifest.Init(wd)
			if err != nil {
				return err
			}
			if created {
				fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", filepath.Join(wd, manifest.ManifestFile))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s already exists\n", manifest.ManifestFile)
			}
			return nil
		},
	}
}

func versionCmd() *cobra.Command {
	var verbose bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the binary version and the frozen contract version",
		Run: func(cmd *cobra.Command, _ []string) {
			// buildinfo.Render owns the output shape: the default (non-verbose)
			// line is byte-for-byte the historical format; -v appends the embedded
			// version-pin manifest and dependency versions.
			fmt.Fprint(cmd.OutOrStdout(), buildinfo.Render(version, commit, protocol.ContractVersion, verbose))
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "also print the version-pin manifest (go/platforms/git/adapter toolchain) and module versions")
	return cmd
}
