package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
)

// START-IF-ABSENT (chafe C12): restore the ambient UX — a governed launch should
// not require the operator to have started the daemon by hand — WITHOUT weakening
// the fail-closed guarantee (Inv 4). ensureDaemon is the SINGLE auto-start path
// shared by the shim dispatch and `mad-substrate launch`:
//
//   - If a daemon already holds the flock for the socket, do nothing.
//   - Otherwise spawn THIS binary (`mad-substrate daemon`) in the background with
//     cwd = the current working dir, and wait (bounded) for the socket to accept.
//     The flock single-instance path dedupes a race: a concurrent auto-start (or
//     a hand-started daemon) simply loses the bind and exits; we still observe
//     the winner's socket come up.
//   - If we cannot spawn, or the socket is not ready in time, we RETURN AN ERROR.
//     The caller MUST treat that as a BLOCK (exit 126) exactly as a daemon-down
//     condition is treated today — the agent is never run ungoverned.
//
// This function NEVER runs the agent and NEVER bypasses governance; it only makes
// the daemon present (or fails closed).

// autostartWaitTimeout bounds how long we wait for an auto-started daemon's
// socket to accept after spawning it.
const autostartWaitTimeout = 20 * time.Second

// daemonReady reports whether the socket currently accepts a connection.
func daemonReady(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ensureDaemon guarantees a daemon is reachable on socket, auto-starting one if
// absent. Returns nil when a daemon is ready to dial; a non-nil error means the
// CALLER must fail closed (BLOCK) — never proceed to run the agent.
//
// logf receives a single human line on the auto-start attempt; pass a no-op to
// silence. selfBin is os.Executable() (the binary to re-spawn as the daemon).
func ensureDaemon(socket, selfBin string, logf func(string, ...any)) error {
	// Fast path: a daemon already holds the single-instance flock.
	running, rerr := daemon.IsRunning(socket)
	if rerr != nil {
		// A real lock-probe fault (e.g. a permission problem on "<socket>.lock"):
		// surface it as an immediate fail-closed BLOCK reason rather than swallowing
		// it and deferring to a readiness timeout (matches `daemon status`/`stop`).
		return fmt.Errorf("cannot probe daemon lock for %s: %w", socket, rerr)
	}
	if running {
		// A daemon holds the flock. Confirm it actually ANSWERS an RPC before
		// proceeding: a bare listener-up check would let a wedged-but-bound daemon
		// pass here and defer the catch to a ~120s whoami hang downstream. We cannot
		// auto-start a second instance (the flock is held), so an unresponsive
		// holder is a precise fail-closed error.
		if daemonResponsive(socket) {
			return nil
		}
		return fmt.Errorf("daemon at %s holds the lock but is not responding (diag.health timed out); stop it with `mad-substrate daemon stop` and retry", socket)
	}
	if selfBin == "" {
		return fmt.Errorf("cannot auto-start daemon: the mad-substrate binary path is unknown")
	}
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cannot auto-start daemon: %w", err)
	}

	logf("mad-substrate: daemon not running — auto-starting...")

	cmd := exec.Command(selfBin, "daemon", "--socket", socket)
	cmd.Dir = wd // RepoRoot: the daemon loads the manifest + derives the ledger here
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot auto-start daemon (spawn failed): %w", err)
	}
	// Detach: we do not wait on the daemon (it runs for its whole life). Reap the
	// handle in the background so a fast-failing spawn (e.g. bogus binary that
	// exits immediately) does not linger as a zombie.
	go func() { _ = cmd.Wait() }()

	// NOTE (concurrent auto-start): if several launches race here, each spawns a
	// daemon but the single-instance flock admits exactly ONE; the losers exit.
	// (Today a cold-start loser may fail on durable-resource contention — the
	// ledger/trunk are opened before the flock in the daemon command — so its
	// discarded stderr can show a benign SQLITE_BUSY / "File exists"; safety is
	// unaffected. Acquiring the flock before those opens is a noted follow-up.)
	// We simply wait for the winner's daemon to become RESPONSIVE.
	deadline := time.Now().Add(autostartWaitTimeout)
	for time.Now().Before(deadline) {
		if daemonResponsive(socket) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("auto-started daemon did not become ready (responsive to diag.health) within %s", autostartWaitTimeout)
}

// daemonResponsive reports whether a daemon on socket ANSWERS a cheap, read-only
// diag.health RPC within a short bound — a stronger check than daemonReady's bare
// listener probe: it catches a wedged-but-bound daemon (accepts connections but
// never replies) in ~2s instead of deferring to the downstream 120s whoami hang.
// diag.health is a registered, no-side-effect method, so this adds no RPC surface.
func daemonResponsive(socket string) bool {
	cl, err := rpcclient.Dial(socket, rpcclient.WithDialTimeout(1*time.Second), rpcclient.WithReadTimeout(2*time.Second))
	if err != nil {
		return false
	}
	defer cl.Close()
	var h struct {
		PID int `json:"pid"`
	}
	return cl.Call("diag.health", map[string]any{}, &h) == nil
}
