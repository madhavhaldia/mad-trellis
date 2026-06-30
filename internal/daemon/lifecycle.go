package daemon

// Daemon-process lifecycle helpers (chafe C14): the FLOCK on "<socket>.lock" is
// the single-instance source of truth for the LIVE-HOLDER guarantee — a live
// daemon holds it for its whole life (see acquireSocket), so probing it answers
// "is a daemon running?" authoritatively. (NOTE: the daemon command opens the
// durable ledger/trunk BEFORE acquiring this flock, so a simultaneous cold-start
// loser may fail on durable-resource contention before it ever reaches the flock;
// acquiring the flock first is a noted follow-up. That ordering does not weaken
// the live-holder guarantee or fail-closed.) These helpers let an out-of-process
// CLI answer running/not (probe the flock) and "which pid is it?" (read the
// pidfile the daemon wrote on Start), WITHOUT adding any RPC and WITHOUT trusting
// a self-reported pid over a socket.

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// PidfilePath is the daemon's authoritative pidfile, a sibling of the socket
// (mirrors the ".lock" convention). It records os.Getpid() of the live daemon.
func PidfilePath(socketPath string) string { return socketPath + ".pid" }

// LockPath is the single-instance flock file, a sibling of the socket. A LIVE
// daemon holds an exclusive flock on it for its whole life (acquireSocket).
func LockPath(socketPath string) string { return socketPath + ".lock" }

// writePidfile records pid in the daemon's authoritative pidfile (owner-only,
// matching the socket/lock 0600 perms). Truncates any prior content.
func writePidfile(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// ReadPidfile returns the pid recorded in the daemon's pidfile. It reports
// (0, false, nil) when the pidfile is ABSENT (no daemon ever wrote it, or a
// clean Close removed it) so a caller can treat absence as "not running"
// without special-casing os.IsNotExist. A present-but-garbled pidfile is a
// genuine error (it should never happen for a pidfile this process wrote).
func ReadPidfile(path string) (pid int, present bool, err error) {
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, false, nil
		}
		return 0, false, rerr
	}
	p, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil {
		return 0, true, perr
	}
	return p, true, nil
}

// IsRunning reports whether a LIVE daemon owns socketPath, decided by the FLOCK
// (the true single-instance signal), not by a socket probe or a pidfile:
//
//   - flock contended (EWOULDBLOCK) => a live daemon holds it => running.
//   - flock acquired                => NO live daemon exists. We release it
//     immediately and clean up a STALE pidfile left by a crash, then report
//     not-running. (A live daemon would still hold its flock, so a leftover
//     pidfile here is provably stale.)
//
// A genuine error opening the lock file (e.g. a permission problem) is returned
// so the caller can surface it rather than silently report "not running".
func IsRunning(socketPath string) (running bool, err error) {
	lockPath := LockPath(socketPath)
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer lf.Close()
	if ferr := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); ferr != nil {
		if errors.Is(ferr, syscall.EWOULDBLOCK) {
			return true, nil // a live daemon holds the flock
		}
		return false, ferr
	}
	// Sole acquirer: no live daemon. Clean up a stale pidfile, then release.
	_ = os.Remove(PidfilePath(socketPath))
	_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	return false, nil
}
