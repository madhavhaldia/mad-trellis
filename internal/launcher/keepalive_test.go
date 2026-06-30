package launcher

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scriptKeepaliver is a controllable sessionKeepaliver: the test scripts what each
// RenewSessionLease / Reattach call returns (by call number) and reads the counts.
type scriptKeepaliver struct {
	mu       sync.Mutex
	renewN   int
	attachN  int
	renew    func(n int) (bool, error)
	reattach func(n int) error
}

func (k *scriptKeepaliver) RenewSessionLease(_ string, _ time.Duration) (bool, error) {
	k.mu.Lock()
	k.renewN++
	n := k.renewN
	f := k.renew
	k.mu.Unlock()
	return f(n)
}

func (k *scriptKeepaliver) Reattach(_ string) error {
	k.mu.Lock()
	k.attachN++
	n := k.attachN
	f := k.reattach
	k.mu.Unlock()
	return f(n)
}

func (k *scriptKeepaliver) counts() (renew, attach int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.renewN, k.attachN
}

// fastKeepalive shrinks the recovery backoff so the test runs quickly; it restores
// the originals on cleanup.
func fastKeepalive(t *testing.T) {
	t.Helper()
	ob, oa := keepaliveRecoverBackoff, keepaliveRecoverAttempts
	keepaliveRecoverBackoff = 1 * time.Millisecond
	keepaliveRecoverAttempts = 50
	t.Cleanup(func() { keepaliveRecoverBackoff, keepaliveRecoverAttempts = ob, oa })
}

// TestKeepaliveRecoversAfterDaemonRestart is the P0 #4 core: when renew starts
// failing (a daemon restart reset the connection identity), the loop RE-ATTACHES via
// the token and resumes renewing — it does NOT give up (the old behavior, which let
// the lease lapse and the boundary get reclaimed).
func TestKeepaliveRecoversAfterDaemonRestart(t *testing.T) {
	fastKeepalive(t)
	var reattached atomic.Bool
	recovered := make(chan struct{}, 1)

	k := &scriptKeepaliver{
		renew: func(n int) (bool, error) {
			if reattached.Load() {
				return true, nil // after re-attach, renews succeed
			}
			// Pre-reattach: the daemon restarted, our connection is a fresh identity →
			// renew is not the holder (ok=false) / or a transport blip.
			return false, errors.New("renew failed: connection reset by daemon restart")
		},
		reattach: func(n int) error {
			reattached.Store(true)
			select {
			case recovered <- struct{}{}:
			default:
			}
			return nil
		},
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSessionKeepalive(k, "key", "tok", 4*time.Millisecond, stop, func(string, ...any) {})
	}()

	// The loop must re-attach after renew starts failing.
	select {
	case <-recovered:
	case <-time.After(2 * time.Second):
		close(stop)
		t.Fatal("keepalive did not re-attach after renew failures")
	}

	// After recovery, renews must keep succeeding WITHOUT further re-attaches (the
	// session is healthy again). Sample the attach count, wait, and assert it is stable
	// while renews advance.
	_, attachAfterRecover := k.counts()
	time.Sleep(40 * time.Millisecond)
	renewLater, attachLater := k.counts()
	if attachLater != attachAfterRecover {
		t.Fatalf("no further re-attach should happen once healthy: attach went %d -> %d", attachAfterRecover, attachLater)
	}
	if renewLater < 2 {
		t.Fatalf("renews should continue after recovery; got %d", renewLater)
	}

	// Clean stop returns promptly.
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive did not return after stop")
	}
}

// TestKeepaliveKeepsRetryingWhileReattachFails: a re-attach that fails transiently
// (daemon still coming back) does NOT end the loop — it keeps retrying and recovers
// once re-attach finally succeeds.
func TestKeepaliveKeepsRetryingWhileReattachFails(t *testing.T) {
	fastKeepalive(t)
	var reattached atomic.Bool
	recovered := make(chan struct{}, 1)

	k := &scriptKeepaliver{
		renew: func(n int) (bool, error) {
			if reattached.Load() {
				return true, nil
			}
			return false, errors.New("renew failed")
		},
		reattach: func(n int) error {
			if n < 5 {
				return errors.New("daemon still unreachable") // transient
			}
			reattached.Store(true)
			select {
			case recovered <- struct{}{}:
			default:
			}
			return nil
		},
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSessionKeepalive(k, "key", "tok", 4*time.Millisecond, stop, func(string, ...any) {})
	}()

	select {
	case <-recovered:
	case <-time.After(2 * time.Second):
		close(stop)
		t.Fatal("keepalive gave up instead of retrying re-attach until it succeeded")
	}
	if _, attachN := k.counts(); attachN < 5 {
		t.Fatalf("expected >=5 re-attach attempts before success; got %d", attachN)
	}
	close(stop)
	<-done
}

// TestKeepaliveStopsPromptlyOnHealthyPath: with renews always succeeding, closing
// stop returns the loop without churn.
func TestKeepaliveStopsPromptlyOnHealthyPath(t *testing.T) {
	fastKeepalive(t)
	k := &scriptKeepaliver{
		renew:    func(int) (bool, error) { return true, nil },
		reattach: func(int) error { return nil },
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSessionKeepalive(k, "key", "tok", 10*time.Millisecond, stop, func(string, ...any) {})
	}()
	time.Sleep(30 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive did not return after stop on the healthy path")
	}
	if _, attachN := k.counts(); attachN != 0 {
		t.Fatalf("no re-attach should happen on a healthy path; got %d", attachN)
	}
}
