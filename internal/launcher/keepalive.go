package launcher

import "time"

// keepalive.go is the P0 #4 fix: keep a launcher's session-liveness lease alive
// ACROSS A DAEMON RESTART so liveness never reclaims a still-running session's
// boundary.
//
// THE BUG IT FIXES: the launcher holds ONE daemon connection = its session, and
// renews the session-liveness lease at ~TTL/2. When the DAEMON restarts, that
// connection drops; the auto-reconnecting RPC client transparently re-dials the new
// daemon — but the new connection gets a FRESH daemon identity, so the next
// lease.renew (holder-bound to the connection's identity, Inv 4) is NOT the lease
// holder and returns ok=false. The old loop then STOPPED renewing → the lease
// lapsed → liveness on the restarted daemon reclaimed the boundary out from under
// the live agent (and for the container grain, KILLED the container). The fix:
// on any renew failure, RE-ATTACH via the T2 capability token (which resolves
// against the DURABLE token store the restarted daemon reloaded), rebinding the
// connection to the ORIGINAL identity, then renew — so the lease never lapses.

// sessionKeepaliver is the minimal session surface the keepalive loop drives. It is
// an interface so the loop is unit-testable with a fake that simulates a daemon
// restart (renew fails, then re-attach recovers). *Session implements it.
type sessionKeepaliver interface {
	RenewSessionLease(livenessKey string, ttl time.Duration) (ok bool, err error)
	Reattach(token string) error
}

// Recovery cadence: on a renew failure, re-attach+renew is retried up to
// keepaliveRecoverAttempts times with keepaliveRecoverBackoff between tries before
// falling back to the normal renew tick — so a fast daemon restart is recovered in
// ~a second (well inside the lease TTL) while a slower restart is retried each tick.
// Package vars so a test can shrink them.
var (
	keepaliveRecoverAttempts = 12
	keepaliveRecoverBackoff  = 1 * time.Second
)

// runSessionKeepalive keeps the session-liveness lease alive until stop is closed.
// Each renewEvery (~TTL/2) it renews; on ANY failure (a transient blip, or a daemon
// RESTART that reset this connection's identity) it RECOVERS by re-attaching via the
// capability token then renewing as the restored identity. It never gives up on its
// own — it is bounded only by stop (clean exit) and, for the container grain, by the
// agent dying when its boundary is reclaimed (which ends the launcher). Renewing
// before the lease TTL elapses is what keeps liveness from reclaiming a live session.
func runSessionKeepalive(s sessionKeepaliver, livenessKey, token string, ttl time.Duration, stop <-chan struct{}, logf func(string, ...any)) {
	renewEvery := ttl / 2
	if renewEvery <= 0 {
		renewEvery = ttl
	}
	t := time.NewTimer(renewEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		if ok, err := s.RenewSessionLease(livenessKey, ttl); err == nil && ok {
			t.Reset(renewEvery)
			continue
		}
		// Renew failed — recover (the daemon may have restarted, resetting our
		// connection identity). Re-attach via the token, then renew as the restored
		// identity. Tolerant: if recovery does not succeed this round we retry at the
		// next tick rather than abandoning a possibly-still-live session.
		if recovered := recoverSession(s, livenessKey, token, ttl, stop, logf); !recovered {
			if interrupted(stop) {
				return
			}
			logf("session-liveness: could not re-attach this round; will retry at the next tick")
		}
		t.Reset(renewEvery)
	}
}

// recoverSession retries re-attach+renew with a short backoff. It returns true once
// a renew succeeds after a successful re-attach; false if it exhausts its attempts
// or stop fires.
func recoverSession(s sessionKeepaliver, livenessKey, token string, ttl time.Duration, stop <-chan struct{}, logf func(string, ...any)) bool {
	for i := 0; i < keepaliveRecoverAttempts; i++ {
		if interrupted(stop) {
			return false
		}
		if rerr := s.Reattach(token); rerr != nil {
			logf("session-liveness: re-attach attempt %d/%d failed: %v", i+1, keepaliveRecoverAttempts, rerr)
		} else if ok, err := s.RenewSessionLease(livenessKey, ttl); err == nil && ok {
			logf("session-liveness: re-attached + renewed after a daemon restart")
			return true
		}
		if sleepInterruptible(stop, keepaliveRecoverBackoff) {
			return false
		}
	}
	return false
}

// interrupted reports whether stop is already closed (non-blocking).
func interrupted(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// sleepInterruptible sleeps for d or until stop closes; returns true if interrupted.
func sleepInterruptible(stop <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
		return true
	case <-t.C:
		return false
	}
}
