package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-trellis/internal/launcher"
	"github.com/madhavhaldia/mad-trellis/internal/runtimecfg"
)

// launchCmd is the transparent governed launcher (project 5). `mad-trellis launch
// -- <agent> [args...]` provisions an isolation boundary, then execs the OPAQUE
// agent into it on a PTY and tears the boundary down on clean exit. It is what a
// shimmed agent invocation routes through.
//
// NEGATIVE OBLIGATION (Inv 13, no-goals/no-dispatch): the ONLY inputs are the
// daemon socket, a port count, and the opaque agent command after `--`. There is
// deliberately no --goal/--task/--prompt affordance; everything after `--` is the
// agent's own command line, forwarded verbatim. (launcher.AuditNoGoals pins this
// in the tests with a positive control.)
func launchCmd() *cobra.Command {
	var socket string
	var ports int
	var grain string
	cmd := &cobra.Command{
		Use:                "launch [--grain worktree|container] [-- <agent> args...]",
		Short:              "Run a supported agent CLI inside a governed isolation boundary (transparent)",
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("usage: mad-trellis launch -- <agent> [args...]")
			}
			socket = runtimecfg.SocketPath(socket)
			// GRAIN DIAL (Inv 10): --grain selects the isolation backend. The flag
			// wins over the inherited MAD_GRAIN. The daemon reads MAD_GRAIN
			// at construction, so we export the choice into THIS process's env BEFORE
			// the auto-start path (ensureDaemon) spawns a daemon — an auto-started
			// daemon inherits it and provisions that grain. The launcher's exec path is
			// then driven by the grain the daemon REPORTS on the wire, so it confines
			// correctly regardless of how the daemon was started.
			if err := applyGrainSelection(grain); err != nil {
				return err
			}
			// args[0] is the agent (a bare name or an explicit path); the rest are
			// its own args. governedLaunch resolves + runs fail-closed and exits with
			// the child's code (or BlockedExitCode), so it never returns.
			governedLaunch(args[0], args[1:], socket, ports)
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", socketFlagHelp)
	cmd.Flags().IntVar(&ports, "ports", 0, "ports to allocate for the boundary (0 → substrate default)")
	cmd.Flags().StringVar(&grain, "grain", "", grainFlagHelp)
	return cmd
}

// grainFlagHelp is the shared --grain help across launch/spawn.
const grainFlagHelp = "isolation grain: worktree (default) | container (structural confinement); honors $MAD_GRAIN when unset"

// applyGrainSelection validates the requested grain and exports it as
// MAD_GRAIN so an auto-started daemon provisions it (the registry is frozen,
// so the grain is carried via env, not a new RPC param). An empty flag leaves any
// inherited MAD_GRAIN untouched (so the env-only path still works). Unknown
// grains are rejected up front (fail-closed) rather than deferred to a confusing
// daemon-side provision error.
func applyGrainSelection(grain string) error {
	g := strings.TrimSpace(grain)
	if g == "" {
		return nil // honor inherited MAD_GRAIN (or the default)
	}
	switch strings.ToLower(g) {
	case "worktree", "container":
		return os.Setenv("MAD_GRAIN", strings.ToLower(g))
	default:
		return fmt.Errorf("unknown --grain %q (want worktree|container)", grain)
	}
}

// governedLaunch runs the agent under governance, exiting with the child's exit
// code — or BlockedExitCode if a governed session could not be established. It
// never returns. The agent is NOT resolved here: launcher.Run resolves it on the
// grain the daemon AUTHORITATIVELY reports (host → shim-excluded resolveAgentBinary;
// container → in-container), so resolution and the exec grain never diverge and a
// bare name can't re-exec our own shim (the C34 / shim-loop fix).
func governedLaunch(agentArg string, passArgs []string, socket string, ports int) {
	logf := func(string, ...any) {}
	if os.Getenv("MAD_DEBUG") != "" {
		logf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, "mad-trellis: "+f+"\n", a...) }
	}

	// FAIL-FAST: mad-trellis governs a GIT REPOSITORY — every grain provisions a
	// git worktree/clone and the trunk is a git ref. If cwd is not inside a repo,
	// BLOCK here with an actionable message, BEFORE auto-starting a daemon or
	// offering an integrator terminal — otherwise those side effects fire and the
	// launch still fails late on the boundary provision (`git worktree add: not a
	// git repository`), stranding a stray integrator window.
	if !cwdInGitRepo() {
		wd, _ := os.Getwd()
		fmt.Fprintf(os.Stderr,
			"mad-trellis: BLOCKED: %s is not a git repository — mad-trellis governs a git repo "+
				"(each agent runs in its own `git worktree`). Run `git init` here, or cd into a repo; "+
				"refusing to launch %q ungoverned\n", wd, agentArg)
		os.Exit(launcher.BlockedExitCode)
	}

	// START-IF-ABSENT (C12): if the daemon is unreachable, attempt a bounded
	// auto-start (shared with the shim path — both reach here). This restores the
	// ambient UX while staying FAIL-CLOSED: if the daemon cannot be made present,
	// we BLOCK exactly as launcher.Run would, and the agent never runs. The
	// auto-start logs "daemon not running — auto-starting..." regardless of
	// MAD_DEBUG so the operator sees why there is a brief pause.
	self, selfErr := os.Executable()
	if selfErr != nil {
		self = "" // ensureDaemon turns an unknown self into a clean BLOCK
	}
	if err := ensureDaemon(socket, self, func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }); err != nil {
		fmt.Fprintf(os.Stderr, "mad-trellis: BLOCKED: %v; refusing to launch %q ungoverned\n", err, agentArg)
		os.Exit(launcher.BlockedExitCode)
	}

	// WING 3 UX (best-effort, never blocking): if no integrator is running for
	// this repo and we are on an interactive terminal, offer to launch one in a
	// new terminal before the builder starts. This is pure UX — it never returns
	// an error and never aborts the launch (headless/piped launches never prompt).
	maybePromptIntegrator(socket, agentArg)

	code, rerr := launcher.Run(launcher.Config{
		Socket:                  socket,
		Agent:                   agentArg,
		Args:                    passArgs,
		Ports:                   ports,
		Logf:                    logf,
		RequestedContainerGrain: effectiveGrainIsContainer(),
		ResolveAgent: func(a string) (string, error) {
			return resolveAgentBinary(a, socket)
		},
	})
	if rerr != nil {
		// On a BLOCK this carries the reason; on a clean run rerr is nil.
		fmt.Fprintf(os.Stderr, "mad-trellis: %v\n", rerr)
	}
	os.Exit(code)
}

// resolveAgentBinary turns the agent argument into the real executable to run,
// fail-closed. An explicit path (containing a separator) is used as-is after an
// executable check; a bare name is resolved on PATH EXCLUDING the shim dir, so
// the launcher never re-execs its own shim (infinite loop).
func resolveAgentBinary(agentArg, socket string) (string, error) {
	if strings.ContainsRune(agentArg, os.PathSeparator) {
		fi, err := os.Stat(agentArg)
		if err != nil {
			return "", fmt.Errorf("agent %q not found: %w", agentArg, err)
		}
		if fi.IsDir() || fi.Mode()&0o111 == 0 {
			return "", fmt.Errorf("agent %q is not executable", agentArg)
		}
		return agentArg, nil
	}
	// FAIL-CLOSED: if we cannot identify our own binary, we cannot reliably
	// exclude the shim from PATH resolution — so BLOCK rather than risk resolving
	// the agent name back to the shim and re-exec'ing mad-trellis forever.
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot identify the mad-trellis binary to exclude the shim: %w", err)
	}
	shimDir := launcher.DefaultShimDir(filepath.Dir(socket))
	return launcher.ResolveReal(agentArg, shimDir, self, os.Getenv("PATH"))
}

// effectiveGrainIsContainer reports whether the REQUESTED isolation grain is the
// container grain, read from MAD_GRAIN (which `launch` exports from --grain
// via applyGrainSelection, and the shim path inherits). It drives ONLY the agent-
// resolution choice above; the launcher's exec path is still gated on the grain the
// daemon AUTHORITATIVELY reports (spec.Grain), so a grain mismatch can only produce
// a clean non-run, never an ungoverned one.
func effectiveGrainIsContainer() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("MAD_GRAIN")), containerGrainName)
}

// containerGrainName mirrors the substrate/launcher wire value for the container
// grain (kept local so cmd does not import an internal grain constant).
const containerGrainName = "container"

// maybePromptIntegrator offers, on an interactive launch, to start an integrator
// in a new terminal when none is currently running for the repo. It is pure
// best-effort UX: it NEVER returns an error and NEVER blocks or aborts the
// builder launch — governedLaunch proceeds to launcher.Run regardless.
//
// It stays SILENT (no prompt, reads nothing from stdin) when ANY of:
//   - the opt-out env MAD_INTEGRATOR_PROMPT is off/0/false/no,
//   - stdin is not an interactive terminal (headless / `-p` / piped / CI / tests),
//   - an integrator already holds the singleton presence lease,
//   - the presence check errored (daemon unreachable — fail-soft, can't tell).
func maybePromptIntegrator(socket, agent string) {
	if integratorPromptDisabled(os.Getenv("MAD_INTEGRATOR_PROMPT")) {
		return
	}
	// Only prompt on a real interactive terminal (stdlib-only TTY detection). A
	// non-char-device stdin (pipe, file, /dev/null, `claude -p`) never prompts.
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return
	}
	held, _, perr := integratorPresent(socket)
	if perr != nil || held {
		return // can't tell (daemon unreachable) or one is already running
	}

	fmt.Fprint(os.Stderr,
		"No integrator is running for this repo. Launch one in a new terminal to review & merge agent work? [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		dir, _ := os.Getwd()
		// Default the integrator agent to the SAME agent being launched.
		if _, serr := startIntegrator(socket, dir, agent, nil, false); serr != nil {
			fmt.Fprintf(os.Stderr, "mad-trellis: could not start integrator: %v (continuing without one)\n", serr)
		}
	default:
		// Empty / EOF / anything but y/yes = no. Builder launches without one.
	}
}

// integratorPromptDisabled reports whether the launch-time integrator prompt is
// opted out via MAD_INTEGRATOR_PROMPT. off/0/false/no (case-insensitive,
// trimmed) disable it; everything else (incl. "", "on", "1", garbage) leaves it
// enabled. Pure so it is exhaustively unit-testable.
func integratorPromptDisabled(env string) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "off", "0", "false", "no":
		return true
	default:
		return false
	}
}

// shimCmd installs and inspects the transparent shim (PATH interposition).
func shimCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shim",
		Short: "Install or inspect the transparent agent shim (PATH interposition)",
	}
	cmd.AddCommand(shimInstallCmd(), shimStatusCmd())
	return cmd
}

func shimInstallCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "install [agents...]",
		Short: "Install shims so launching a supported agent auto-runs it governed",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dir == "" {
				dir = launcher.DefaultShimDir(defaultRuntimeDir())
			}
			agents := args
			if len(agents) == 0 {
				agents = launcher.SupportedAgents
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			installed, err := launcher.InstallShims(self, dir, agents)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "installed %d shim(s) in %s:\n", len(installed), dir)
			for _, p := range installed {
				fmt.Fprintf(out, "  %s\n", filepath.Base(p))
			}
			// PATH interposition is OPT-IN — we never edit the user's shell rc. Print
			// the line to eval so the shim takes precedence over the real agents.
			fmt.Fprintf(out, "\nAdd the shim dir to the FRONT of PATH (eval or add to your shell rc):\n")
			fmt.Fprintf(out, "  export PATH=\"%s:$PATH\"\n", dir)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "shim directory (default ~/.mad-trellis/shims)")
	return cmd
}

func shimStatusCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether the shim is installed and ahead of the real agents on PATH",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" {
				dir = launcher.DefaultShimDir(defaultRuntimeDir())
			}
			out := cmd.OutOrStdout()
			pathDirs := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))
			onPath := false
			for _, d := range pathDirs {
				if d == dir {
					onPath = true
					break
				}
			}
			fmt.Fprintf(out, "shim dir: %s\n", dir)
			fmt.Fprintf(out, "on PATH:  %v\n", onPath)
			for _, a := range launcher.SupportedAgents {
				_, err := os.Stat(filepath.Join(dir, a))
				fmt.Fprintf(out, "  %-8s installed=%v\n", a, err == nil)
			}
			if !onPath {
				fmt.Fprintf(out, "\nShim not on PATH — governed interception is NOT active. Run `mad-trellis shim install` and add it to PATH.\n")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "shim directory (default ~/.mad-trellis/shims)")
	return cmd
}
