package launcher

import (
	"errors"
	"testing"
)

// inspectConn returns a scripted lease.inspect reply (or an error) for the
// mechanical-enforcement tests.
type inspectConn struct {
	reply map[string]any
	err   error
}

func (c *inspectConn) Call(method string, _ any, out any) error {
	if method != "lease.inspect" {
		return errors.New("unexpected method: " + method)
	}
	if c.err != nil {
		return c.err
	}
	return assign(out, c.reply)
}
func (c *inspectConn) Close() error { return nil }

var trunkKey = []byte("mad-substrate:trunk:v1")

// EnforceLeaseHeld is the mechanical boundary (Inv 4): the ONLY path to "allowed"
// is an explicit, current, same-session hold. Everything else — including every
// uncertainty — is a deny (fail-closed, Inv 9).

func TestEnforceAllowsCurrentSameSessionHold(t *testing.T) {
	c := &inspectConn{reply: map[string]any{"exists": true, "held": true, "holder": "s-1-me"}}
	if err := EnforceLeaseHeld(c, "s-1-me", trunkKey); err != nil {
		t.Fatalf("a current same-session hold must be allowed, got %v", err)
	}
}

func TestEnforceBlocksNoLease(t *testing.T) {
	c := &inspectConn{reply: map[string]any{"exists": false}}
	if err := EnforceLeaseHeld(c, "s-1-me", trunkKey); !errors.Is(err, ErrBlocked) {
		t.Fatalf("no lease ⇒ no action; want ErrBlocked, got %v", err)
	}
}

func TestEnforceBlocksHeldByAnotherSession(t *testing.T) {
	c := &inspectConn{reply: map[string]any{"exists": true, "held": true, "holder": "s-2-other"}}
	if err := EnforceLeaseHeld(c, "s-1-me", trunkKey); !errors.Is(err, ErrBlocked) {
		t.Fatalf("a lease held by another session must block us; got %v", err)
	}
}

func TestEnforceBlocksExpiredLease(t *testing.T) {
	c := &inspectConn{reply: map[string]any{"exists": true, "held": false, "holder": "s-1-me"}}
	if err := EnforceLeaseHeld(c, "s-1-me", trunkKey); !errors.Is(err, ErrBlocked) {
		t.Fatalf("an expired (not-held) lease must block; got %v", err)
	}
}

// FAIL-CLOSED: uncertainty about ledger state is a deny, never a pass.
func TestEnforceBlocksOnDaemonError(t *testing.T) {
	c := &inspectConn{err: errors.New("ledger unreachable")}
	if err := EnforceLeaseHeld(c, "s-1-me", trunkKey); !errors.Is(err, ErrBlocked) {
		t.Fatalf("a ledger error must FAIL CLOSED (block); got %v", err)
	}
}

func TestEnforceBlocksIncompleteContext(t *testing.T) {
	c := &inspectConn{reply: map[string]any{"exists": true, "held": true, "holder": "s-1-me"}}
	if err := EnforceLeaseHeld(c, "", trunkKey); !errors.Is(err, ErrBlocked) {
		t.Errorf("empty session must block; got %v", err)
	}
	if err := EnforceLeaseHeld(c, "s-1-me", nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("empty key must block; got %v", err)
	}
	if err := EnforceLeaseHeld(nil, "s-1-me", trunkKey); !errors.Is(err, ErrBlocked) {
		t.Errorf("nil conn must block; got %v", err)
	}
}

// POSITIVE CONTROL: a permissive gate that always allows would let the no-lease
// case through. Asserting it allows where EnforceLeaseHeld blocks proves the
// no-lease/uncertainty tests above are catching a real difference, not passing
// vacuously.
func TestEnforceControlPermissiveWouldAllowUngoverned(t *testing.T) {
	permissive := func(Conn, string, []byte) error { return nil } // the BUG we forbid
	c := &inspectConn{reply: map[string]any{"exists": false}}

	if permissive(c, "s-1-me", trunkKey) != nil {
		t.Fatal("control is malformed: the permissive gate should allow")
	}
	if err := EnforceLeaseHeld(c, "s-1-me", trunkKey); err == nil {
		t.Fatal("REGRESSION: EnforceLeaseHeld allowed a no-lease action like the permissive control")
	}
}
