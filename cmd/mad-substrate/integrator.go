package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-substrate/internal/coopwiring"
	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
	"github.com/madhavhaldia/mad-substrate/internal/runtimecfg"
)

// integratorPresenceTTLHint mirrors the MCP server's integratorPresenceTTL (60s)
// for user-facing "will auto-clear within ~Ns" messaging only. It is NOT the
// authority (the server's TTL is) — it just keeps `stop`'s advice honest.
const integratorPresenceTTLHint = 60

// integratorLeaseKey is the FROZEN presence-lease key bytes for the singleton
// integrator. The integrator's MCP server (`mad-substrate mcp --role integrator`,
// a sibling unit) ACQUIRES and holds this lease; this command only INSPECTs it
// to enforce "one integrator per trunk". The lease.inspect param encodes the key
// as standard base64 of these raw bytes, mirroring internal/launcher/enforce.go.
const integratorLeaseKey = "mad-substrate:integrator:v1"

func integratorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "integrator",
		Short: "Launch and inspect the external integrator agent (one per trunk)",
		Long: "The integrator is a normal coding agent (claude/codex) that runs DIRECTLY in the current " +
			"trunk/feature worktree — not an isolated boundary — wired with the integrator MCP toolset " +
			"(`mad-substrate mcp --role integrator`). It reviews builder branches and merges them through the " +
			"gated trunk lease. Exactly one integrator runs per trunk, enforced by a singleton presence lease.",
	}
	cmd.AddCommand(integratorStartCmd(), integratorStatusCmd(), integratorStopCmd())
	return cmd
}

func integratorStartCmd() *cobra.Command {
	var socket string
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "start [-- <agent> args...]",
		Short: "Wire a coding agent as the integrator and open it in a separate terminal",
		Long: "start wires the chosen agent (default: claude; also codex) with the integrator MCP toolset " +
			"(`mad-substrate mcp --role integrator`) into the CURRENT worktree (the trunk/feature checkout), then " +
			"opens it in a new visible terminal with that worktree as cwd. It first inspects the singleton " +
			"integrator presence lease and refuses to start a second one. The daemon launches nothing — this " +
			"is the user-invoked CLI doing it.\n\n" +
			"Terminal mechanism: macOS uses osascript -> Terminal.app; elsewhere $TERMINAL -e, else tmux " +
			"new-window. If none works (or with --print), it PRINTS the exact command to paste and does not fail.",
		RunE: func(cmd *cobra.Command, args []string) error {
			socket = runtimecfg.SocketPath(socket)

			// 1) Presence check: refuse a second integrator. Fail-soft — if the daemon
			// is unreachable we cannot verify the singleton, so we WARN and continue
			// (wiring + terminal still work; the integrator's own lease acquire is the
			// real gate).
			held, holder, reachable := inspectIntegrator(socket)
			if reachable && held {
				fmt.Fprintf(cmd.OutOrStdout(),
					"an integrator is already running (holder %s); only one integrator per trunk\n", holder)
				return nil
			}
			if !reachable {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"warning: daemon not reachable — cannot verify the integrator singleton; continuing")
			}

			// 2) Resolve agent + passthrough args. `integrator start -- codex --foo`
			// gives agent=codex, passthrough=[--foo].
			agent := "claude"
			var passthrough []string
			if len(args) > 0 {
				agent = args[0]
				passthrough = args[1:]
			}

			// 3) Wire the CURRENT worktree (trunk/feature checkout), not a boundary,
			// and open/print it. This is the SAME reusable path the launch-time
			// integrator prompt takes (cmd/mad-substrate/launch.go) — single source of
			// truth for wiring + terminal-open.
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			_, serr := startIntegrator(socket, dir, agent, passthrough, printOnly)
			return serr
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", socketFlagHelp)
	cmd.Flags().BoolVar(&printOnly, "print", false,
		"do not open a terminal; just print the exact command to run (headless/CI)")
	return cmd
}

func integratorStatusCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether an integrator is currently running on this trunk",
		RunE: func(cmd *cobra.Command, _ []string) error {
			socket = runtimecfg.SocketPath(socket)
			// Default (pool size 1): the historic single-line singleton status,
			// byte-identical to before. Opt-in pool (N>1): report slot occupancy.
			if n := integratorPoolSize(); n > 1 {
				running, reachable := inspectIntegratorPool(socket, n)
				fmt.Fprintln(cmd.OutOrStdout(), renderIntegratorPoolStatus(running, n, reachable))
				return nil
			}
			held, holder, reachable := inspectIntegrator(socket)
			fmt.Fprintln(cmd.OutOrStdout(), renderIntegratorStatus(held, holder, reachable))
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", socketFlagHelp)
	return cmd
}

// renderIntegratorStatus is the pure status line, factored out so it is unit
// testable without a daemon.
func renderIntegratorStatus(held bool, holder string, reachable bool) string {
	if !reachable {
		return "no integrator running (daemon not reachable)"
	}
	if held {
		return fmt.Sprintf("integrator running (holder %s)", holder)
	}
	return "no integrator running"
}

// integratorPoolSize mirrors the MCP server's MAD_INTEGRATOR_POOL parse so
// `integrator status` inspects the same slot set the server claims. Default/empty/
// unparseable/<=1 ⇒ 1 (the singleton).
func integratorPoolSize() int {
	v := strings.TrimSpace(os.Getenv("MAD_INTEGRATOR_POOL"))
	if v == "" {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// inspectIntegratorPool inspects each of the n pool slot leases
// (mad-substrate:integrator:v1:slot-0 .. slot-(n-1)) and counts how many are held.
// It returns (running, reachable); reachable is false (fail-soft) when the daemon
// is unreachable or any inspect call fails — callers then report not-reachable.
func inspectIntegratorPool(socket string, n int) (running int, reachable bool) {
	cl, err := rpcclient.Dial(socket)
	if err != nil {
		return 0, false
	}
	defer cl.Close()
	for i := 0; i < n; i++ {
		key := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:slot-%d", integratorLeaseKey, i)))
		var info struct {
			Held bool `json:"held"`
		}
		if err := cl.Call("lease.inspect", map[string]any{"key": key}, &info); err != nil {
			return 0, false
		}
		if info.Held {
			running++
		}
	}
	return running, true
}

// renderIntegratorPoolStatus is the pure pool-occupancy status line, factored out
// for unit testing without a daemon.
func renderIntegratorPoolStatus(running, total int, reachable bool) string {
	if !reachable {
		return "no integrator running (daemon not reachable)"
	}
	return fmt.Sprintf("integrators: %d/%d running", running, total)
}

// inspectIntegrator dials the daemon and inspects the singleton integrator
// presence lease. It returns (held, holder, reachable). reachable is false when
// the daemon cannot be reached or the inspect call fails — the caller treats
// that fail-soft (it never blocks starting/reporting).
func inspectIntegrator(socket string) (held bool, holder string, reachable bool) {
	return inspectKey(socket, base64.StdEncoding.EncodeToString([]byte(integratorLeaseKey)))
}

// inspectKey inspects any lease key (base64 on the wire) via the frozen
// lease.inspect method. It returns (held, holder, reachable); reachable is false
// (fail-soft) when the daemon is unreachable or the inspect fails.
func inspectKey(socket, keyB64 string) (held bool, holder string, reachable bool) {
	cl, err := rpcclient.Dial(socket)
	if err != nil {
		return false, "", false
	}
	defer cl.Close()
	var info struct {
		Holder string `json:"holder"`
		Held   bool   `json:"held"`
	}
	if err := cl.Call("lease.inspect", map[string]any{"key": keyB64}, &info); err != nil {
		return false, "", false
	}
	return info.Held, info.Holder, true
}

// integratorStopCmd stops the running integrator on this trunk and frees its
// singleton presence lease. Because a lease is released ONLY by its own holder or
// by liveness reclaim of a dead one (Inv 4 — one session never releases another's
// lease), stop works by SIGNALLING the integrator's own MCP process: it reads the
// pidfile that process wrote, verifies the pid is really an mad-substrate integrator
// (so a reused pid is never signalled), and sends the SIGTERM the server handles
// by releasing its lease. If the process is already gone, a reclaim pass frees an
// already-expired lease; one whose TTL has not yet lapsed clears on its own.
func integratorStopCmd() *cobra.Command {
	var socket string
	var force bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running integrator and free its presence lease",
		Long: "stop signals the running integrator's MCP server to shut down cleanly, releasing the " +
			"singleton presence lease so a new integrator can start. It finds the process via the pidfile the " +
			"integrator wrote beside the ledger, verifies the pid is really an mad-substrate integrator (a reused " +
			"pid is never signalled), and sends SIGTERM — which the integrator handles by releasing its lease. " +
			"If the process is already gone, stop runs a reclaim pass; a lease whose TTL has not yet lapsed " +
			"clears within ~60s. --force escalates to SIGKILL if a clean stop does not release the lease.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			socket = runtimecfg.SocketPath(socket)
			out := cmd.OutOrStdout()
			acted := false
			for _, raw := range integratorStopSlotKeys(integratorPoolSize()) {
				b64 := base64.StdEncoding.EncodeToString(raw)
				held, holder, reachable := inspectKey(socket, b64)
				if !reachable {
					fmt.Fprintln(cmd.ErrOrStderr(),
						"warning: daemon not reachable — cannot verify or stop the integrator")
					return nil
				}
				if !held {
					continue
				}
				acted = true
				stopOneIntegrator(out, socket, raw, holder, force)
			}
			if !acted {
				fmt.Fprintln(out, "no integrator running")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", socketFlagHelp)
	cmd.Flags().BoolVar(&force, "force", false,
		"escalate to SIGKILL if a clean stop does not release the lease")
	return cmd
}

// integratorStopSlotKeys returns the RAW presence-lease keys to stop: the single
// well-known key for the default singleton (N<=1), or one key per slot for an
// opt-in pool. Mirrors the MCP server's integratorSlotKeys (raw side).
func integratorStopSlotKeys(n int) [][]byte {
	if n <= 1 {
		return [][]byte{[]byte(integratorLeaseKey)}
	}
	keys := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = []byte(fmt.Sprintf("%s:slot-%d", integratorLeaseKey, i))
	}
	return keys
}

// stopOneIntegrator stops the integrator holding one presence slot. rawKey is the
// raw slot key (held, per the caller's inspect); holder is its session id.
func stopOneIntegrator(out io.Writer, socket string, rawKey []byte, holder string, force bool) {
	b64 := base64.StdEncoding.EncodeToString(rawKey)
	pidfile := runtimecfg.IntegratorPidfile(socket, rawKey)
	pid, hasPid := readPidfile(pidfile)

	// Process gone (no/stale pidfile, or the pid is dead): we cannot force-release
	// another session's lease (Inv 4), but a reclaim pass frees it if its TTL has
	// already lapsed; otherwise it self-clears shortly.
	if !hasPid || !pidAlive(pid) {
		_ = os.Remove(pidfile)
		reclaimPass(socket)
		if held, _, reachable := inspectKey(socket, b64); reachable && !held {
			fmt.Fprintf(out, "integrator stopped (holder %s; process had already exited, lease reclaimed)\n", holder)
		} else {
			fmt.Fprintf(out, "integrator process already exited (holder %s); its presence lease auto-clears within ~%ds\n",
				holder, integratorPresenceTTLHint)
		}
		return
	}

	// Pid-reuse guard: never signal a pid the OS has recycled for an unrelated
	// process. If it does not look like an mad-substrate integrator, leave it alone
	// and let the stale lease lapse at its TTL.
	if !pidLooksLikeIntegrator(pid) {
		_ = os.Remove(pidfile)
		fmt.Fprintf(out, "integrator process appears to have exited (holder %s; pid %d was reused); lease auto-clears within ~%ds\n",
			holder, pid, integratorPresenceTTLHint)
		return
	}

	// Send the SIGTERM the integrator MCP server handles: it cancels its context,
	// which releases the presence lease before exit (the tested clean-release path).
	_ = syscall.Kill(pid, syscall.SIGTERM)
	if waitReleased(socket, b64, 3*time.Second) {
		_ = os.Remove(pidfile)
		fmt.Fprintf(out, "stopped integrator (holder %s, pid %d); presence lease released\n", holder, pid)
		return
	}
	if force {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		// SIGKILL skips the clean release; reclaim frees the lease if its TTL lapsed.
		reclaimPass(socket)
		_ = os.Remove(pidfile)
		fmt.Fprintf(out, "force-killed integrator (holder %s, pid %d); lease reclaimed if its TTL lapsed, else clears within ~%ds\n",
			holder, pid, integratorPresenceTTLHint)
		return
	}
	fmt.Fprintf(out, "signalled integrator (holder %s, pid %d) but the lease is still held; retry, or run with --force\n",
		holder, pid)
}

// readPidfile reads a pid from path. ok is false when the file is absent or does
// not contain a positive integer.
func readPidfile(path string) (pid int, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// pidAlive reports whether a process with pid exists (signal 0). EPERM (exists but
// not signal-able) also counts as alive; ESRCH means gone.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// pidLooksLikeIntegrator best-effort verifies (via `ps`) that pid's command is an
// mad-substrate integrator MCP server — the pid-reuse guard so stop never signals a
// recycled pid belonging to an unrelated process. It matches "integrator" (from
// the server's `mcp --role integrator` argv), which is specific to this process
// type; a bare "mad-substrate" match would be too broad (it also hits the CLI
// itself, test binaries, and editors that merely have the repo path open).
func pidLooksLikeIntegrator(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "integrator")
}

// waitReleased polls lease.inspect until the key is free (returns true) or timeout
// elapses (false). Used to confirm a signalled integrator actually released.
func waitReleased(socket, keyB64 string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if held, _, reachable := inspectKey(socket, keyB64); reachable && !held {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// reclaimPass triggers one liveness-recovery pass (the same frozen liveness.scan
// `mad-substrate recover` uses) so an already-expired presence lease is freed
// immediately rather than at the next daemon sweep. Best-effort.
func reclaimPass(socket string) {
	cl, err := rpcclient.Dial(socket)
	if err != nil {
		return
	}
	defer cl.Close()
	var out struct {
		Reclaimed int `json:"reclaimed"`
	}
	_ = cl.Call("liveness.scan", nil, &out)
}

// integratorPresent reports whether an integrator currently holds the singleton
// presence lease. Fail-soft: returns (false, "", err) when the daemon is
// unreachable / the inspect fails — callers treat a non-nil error as "can't tell,
// don't act". A wrapper over inspectIntegrator that maps the reachable=false case
// to an error so the launch-time prompt can fail-soft on it.
func integratorPresent(socket string) (held bool, holder string, err error) {
	held, holder, reachable := inspectIntegrator(socket)
	if !reachable {
		return false, "", fmt.Errorf("daemon not reachable; cannot determine integrator presence")
	}
	return held, holder, nil
}

// startIntegrator wires an integrator agent into dir (the current trunk/feature
// worktree) with the integrator MCP toolset and opens it in a new terminal — or,
// when printOnly is set or no terminal mechanism is available, prints the exact
// command to run. It returns whether a terminal was opened (vs printed) and any
// error. Wiring is fail-soft (a warning, never fatal). This is the single shared
// path used by BOTH `integrator start` and the launch-time integrator prompt
// (maybePromptIntegrator). The socket is accepted for symmetry/future use; the
// presence check is the caller's responsibility (both callers do it before
// reaching here).
func startIntegrator(socket, dir, agent string, passArgs []string, printOnly bool) (opened bool, err error) {
	_ = socket
	host := hostForAgent(agent)
	bin, err := coopwiring.BinaryPath()
	if err != nil {
		return false, fmt.Errorf("resolve mad-substrate binary: %w", err)
	}
	res, werr := coopwiring.WireIntegrator(host, dir, bin)
	if werr != nil {
		// Wiring is best-effort/fail-soft: log and still launch the agent.
		fmt.Fprintf(os.Stderr, "warning: integrator wiring incomplete: %v\n", werr)
	}

	// Build the agent command: agent + injected wiring flags + passthrough.
	agentCmd := make([]string, 0, 1+len(res.ExtraArgs)+len(passArgs))
	agentCmd = append(agentCmd, agent)
	agentCmd = append(agentCmd, res.ExtraArgs...)
	agentCmd = append(agentCmd, passArgs...)

	// Open a terminal (or print the command on fallback / printOnly).
	opened, how := openIntegratorTerminal(dir, agentCmd, printOnly)
	if opened {
		fmt.Fprintf(os.Stdout,
			"launched integrator (%s) in a new terminal via %s\n  cwd: %s\n", agent, how, dir)
	} else {
		fmt.Fprintf(os.Stdout,
			"run the integrator (%s) in a terminal:\n  %s\n", agent, terminalCommandString(dir, agentCmd))
	}
	return opened, nil
}

// hostForAgent maps an agent command (possibly a path) to the coopwiring host
// key. codex -> "codex"; everything else (including claude) -> "claude", the
// default, since claude is the default integrator agent.
func hostForAgent(agent string) string {
	base := strings.ToLower(filepath.Base(agent))
	if strings.Contains(base, "codex") {
		return "codex"
	}
	return "claude"
}

// ---- terminal-open helper (clear seam for tests) ---------------------------

// terminalCommandString renders the single shell command a terminal should run:
// `cd <dir> && <agent ...>`, with every element POSIX single-quote-escaped so a
// dir or arg containing spaces/quotes survives. This is the print-fallback text
// AND the body handed to the platform terminal opener — one source of truth.
func terminalCommandString(dir string, agentCmd []string) string {
	parts := make([]string, 0, len(agentCmd))
	for _, a := range agentCmd {
		parts = append(parts, shellQuote(a))
	}
	return "cd " + shellQuote(dir) + " && " + strings.Join(parts, " ")
}

// shellQuote wraps s in single quotes for POSIX sh, escaping any embedded single
// quote via the '\” idiom. Safe for arbitrary paths/args.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// osaQuote renders s as an AppleScript double-quoted string literal, escaping
// backslashes and double quotes. The inner shell command is already POSIX
// single-quoted, so single quotes pass through literally here.
func osaQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// openIntegratorTerminal opens a new terminal running agentCmd with cwd=dir, and
// returns (opened, mechanism). With printOnly set (or when no mechanism works)
// it opens nothing and returns (false, "") so the caller prints the command —
// it NEVER fails the command for lack of a terminal. The actual window-open is
// not unit-tested (cannot in CI); the command construction + print-fallback are.
func openIntegratorTerminal(dir string, agentCmd []string, printOnly bool) (opened bool, mechanism string) {
	if printOnly {
		return false, ""
	}
	shellCmd := terminalCommandString(dir, agentCmd)

	switch runtime.GOOS {
	case "darwin":
		if path, err := exec.LookPath("osascript"); err == nil {
			script := `tell application "Terminal" to do script ` + osaQuote(shellCmd) + ` activate`
			if exec.Command(path, "-e", script).Run() == nil {
				return true, "Terminal.app"
			}
		}
	default:
		if term := strings.TrimSpace(os.Getenv("TERMINAL")); term != "" {
			// `$TERMINAL -e sh -c '<cmd>'` is the broadly-supported form.
			if exec.Command(term, "-e", "sh", "-c", shellCmd).Start() == nil {
				return true, term
			}
		}
		if path, err := exec.LookPath("tmux"); err == nil {
			if exec.Command(path, "new-window", "-c", dir, shellCmd).Run() == nil {
				return true, "tmux"
			}
		}
	}
	// No mechanism worked: fall back to printing the command (no error).
	return false, ""
}
