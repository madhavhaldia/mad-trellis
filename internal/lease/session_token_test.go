package lease

import (
	"path/filepath"
	"testing"
)

// TestSessionTokenDurableRoundTrip: the durable session-token store (P0 #4)
// persists token-hash -> sessionID across a ledger CLOSE+REOPEN (a daemon restart),
// which is what lets session.attach re-bind a session after the daemon comes back.
func TestSessionTokenDurableRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := l.PutSessionToken("hashA", "s-1-alpha", 1000); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := l.PutSessionToken("hashB", "s-2-beta", 2000); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Overwrite is idempotent (same session re-mint).
	if err := l.PutSessionToken("hashA", "s-1-alpha", 1500); err != nil {
		t.Fatalf("put overwrite: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// REOPEN (the daemon restart): the bindings must survive.
	l2, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()

	id, ok, err := l2.GetSessionToken("hashA")
	if err != nil || !ok || id != "s-1-alpha" {
		t.Fatalf("hashA must survive restart: id=%q ok=%v err=%v", id, ok, err)
	}
	// An unknown hash is (ok=false), never an error.
	if _, ok, err := l2.GetSessionToken("nope"); ok || err != nil {
		t.Fatalf("unknown hash must be (ok=false, nil); got ok=%v err=%v", ok, err)
	}
	// List returns both; created_ms reflects the overwrite.
	rows, err := l2.ListSessionTokens()
	if err != nil || len(rows) != 2 {
		t.Fatalf("list must return 2 rows; got %d err=%v", len(rows), err)
	}
	for _, r := range rows {
		if r.TokenHash == "hashA" && (r.SessionID != "s-1-alpha" || r.CreatedMs != 1500) {
			t.Fatalf("hashA row wrong after overwrite: %+v", r)
		}
	}
	// Delete is idempotent.
	if err := l2.DeleteSessionToken("hashA"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := l2.DeleteSessionToken("hashA"); err != nil {
		t.Fatalf("second delete must be idempotent: %v", err)
	}
	if _, ok, _ := l2.GetSessionToken("hashA"); ok {
		t.Fatal("hashA must be gone after delete")
	}
}
