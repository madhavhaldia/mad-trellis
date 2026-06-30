package main

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
	"github.com/madhavhaldia/mad-substrate/internal/runtimecfg"
)

// daemon stop/status (C14) are PURE-ADDITIVE client-side control commands — they
// add NO daemon RPC. The single-instance source of truth is the FLOCK on
// "<socket>.lock" (a LIVE daemon holds it for its whole life), and the
// authoritative pid is the PIDFILE "<socket>.pid" the daemon writes on Start.
// status/stop decide running/not from the flock and signal the pidfile's pid;
// diag.health is only a best-effort cross-check (uptime/contract), never the
// authority. This replaces the prior self-reported-pid design: a malformed
// responder on the socket can no longer dictate which pid we signal.
//
// TRUST BOUNDARY: the pidfile lives next to the owner-only (0600) socket inside
// the 0700 runtime dir — same-uid owner only, the identical boundary the rest of
// the daemon's authz rests on (v1). A different uid cannot reach it.

// daemonHealth mirrors the fields of diag.health the control commands cross-check
// (uptime/contract). The pid here is the daemon's self-report and is NOT trusted
// as the signal target — that is the pidfile's job.
type daemonHealth struct {
	PID             int    `json:"pid"`
	UptimeSeconds   int64  `json:"uptime_seconds"`
	SocketPath      string `json:"socket_path"`
	ActiveConns     int64  `json:"active_connections"`
	ContractVersion int    `json:"contract_version"`
}

// daemonStatusCmd reports whether a daemon is running on the resolved socket,
// decided by the FLOCK. Exits 0 when running, 1 when not — so scripts can gate on
// it. When running it adds the pid (from the pidfile) and best-effort
// contract/uptime (from diag.health).
func daemonStatusCmd(socket *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether a daemon is running on the resolved socket (exit 0 running, 1 not)",
		Args:  cobra.NoArgs,
		// SilenceUsage so a "not running" exit-1 does not print the usage block.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := runtimecfg.SocketPath(*socket)
			running, err := daemon.IsRunning(s)
			if err != nil {
				return fmt.Errorf("could not probe daemon lock for %s: %w", s, err)
			}
			if !running {
				fmt.Fprintf(cmd.OutOrStdout(), "not running (no daemon holds the lock for %s)\n", s)
				// Non-zero exit without a Go error so cobra prints nothing extra.
				return errSilentExit{code: 1}
			}
			// Running per the flock. The pid is authoritative from the pidfile;
			// contract/uptime are a best-effort dial cross-check.
			pid, present, _ := daemon.ReadPidfile(daemon.PidfilePath(s))
			pidStr := "unknown"
			if present {
				pidStr = fmt.Sprintf("%d", pid)
			}
			contract, uptime, conns := "?", int64(-1), int64(-1)
			if cl, derr := rpcclient.Dial(s); derr == nil {
				var h daemonHealth
				if cerr := cl.Call("diag.health", map[string]any{}, &h); cerr == nil {
					contract = fmt.Sprintf("v%d", h.ContractVersion)
					uptime, conns = h.UptimeSeconds, h.ActiveConns
				}
				cl.Close()
			}
			if uptime >= 0 {
				fmt.Fprintf(cmd.OutOrStdout(),
					"running (pid %s, socket %s, contract %s, uptime %ds, %d active conns)\n",
					pidStr, s, contract, uptime, conns)
			} else {
				// Flock held but diag.health unreachable (daemon mid-start/wedged):
				// still authoritatively "running" from the flock.
				fmt.Fprintf(cmd.OutOrStdout(),
					"running (pid %s, socket %s; diag.health not yet reachable)\n", pidStr, s)
			}
			return nil
		},
	}
}

// daemonStopCmd stops the running daemon authoritatively: it signals the PIDFILE's
// pid with SIGTERM, then confirms the daemon is gone via the FLOCK becoming
// acquirable. A STALE pidfile (pid not alive / flock already free) is reported as
// "not running" and cleaned up, never an error. Idempotent: stopping with nothing
// running succeeds (exit 0).
func daemonStopCmd(socket *string) *cobra.Command {
	return &cobra.Command{
		Use:          "stop",
		Short:        "Stop the running daemon (SIGTERM the pidfile's pid, confirm via the flock)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := runtimecfg.SocketPath(*socket)
			out := cmd.OutOrStdout()

			running, err := daemon.IsRunning(s)
			if err != nil {
				return fmt.Errorf("could not probe daemon lock for %s: %w", s, err)
			}
			if !running {
				// Flock is free => no live daemon. IsRunning already cleaned up any
				// stale pidfile. Idempotent no-op.
				fmt.Fprintf(out, "not running (nothing to stop)\n")
				return nil
			}

			// A daemon holds the flock. Read its authoritative pid from the pidfile.
			pid, present, perr := daemon.ReadPidfile(daemon.PidfilePath(s))
			if perr != nil || !present || pid <= 0 {
				// The flock is held but the pidfile is missing/garbled — we cannot
				// authoritatively name the pid to signal. Fall back to the dial
				// cross-check (best-effort) so a daemon that lost its pidfile is still
				// stoppable; if even that fails, surface a clear error.
				dialedPid, derr := dialPid(s)
				if derr != nil {
					return fmt.Errorf("daemon holds the lock for %s but its pidfile is unusable and diag.health failed (%v); cannot determine pid to stop", s, derr)
				}
				pid = dialedPid
			}

			// Signal the authoritative owner pid. ESRCH (no such process) means a
			// stale pidfile racing a crash — treat as already gone, clean up.
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					_, _ = daemon.IsRunning(s) // re-probe + clean a stale pidfile
					fmt.Fprintf(out, "not running (stale pidfile for pid %d; cleaned up)\n", pid)
					return nil
				}
				return fmt.Errorf("could not signal daemon pid %d: %w", pid, err)
			}

			// Confirm the daemon is gone: the flock becomes acquirable (authoritative)
			// OR the socket refuses (cross-check). Poll up to ~5s.
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if r, _ := daemon.IsRunning(s); !r {
					fmt.Fprintf(out, "stopped daemon (pid %d)\n", pid)
					return nil
				}
				if conn, derr := net.DialTimeout("unix", s, 100*time.Millisecond); derr != nil {
					// Socket refuses; the listener is down. The flock release lags
					// process exit by microseconds — confirm once more, then report.
					if r, _ := daemon.IsRunning(s); !r {
						fmt.Fprintf(out, "stopped daemon (pid %d)\n", pid)
						return nil
					}
				} else {
					conn.Close()
				}
				time.Sleep(50 * time.Millisecond)
			}
			fmt.Fprintf(out, "sent SIGTERM to pid %d but the daemon still holds the lock after 5s\n", pid)
			return errSilentExit{code: 1}
		},
	}
}

// dialPid reaches diag.health and returns the daemon's self-reported pid, used
// ONLY as a fallback when the authoritative pidfile is missing/garbled. A
// non-positive pid is rejected (Kill(0)/Kill(-1) would fire a broad signal).
func dialPid(socket string) (int, error) {
	cl, err := rpcclient.Dial(socket)
	if err != nil {
		return 0, err
	}
	defer cl.Close()
	var h daemonHealth
	if err := cl.Call("diag.health", map[string]any{}, &h); err != nil {
		return 0, err
	}
	if h.PID <= 0 {
		return 0, fmt.Errorf("daemon reported invalid pid %d", h.PID)
	}
	return h.PID, nil
}

// errSilentExit carries a non-zero exit code WITHOUT a noisy "error:" line — the
// status/stop "not running" / timeout messages are already printed to stdout.
type errSilentExit struct{ code int }

func (e errSilentExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }
