package app

// End-to-end test for P0 #4: a session SURVIVES a daemon restart. The launcher
// re-attaches via its capability token (resolved from the DURABLE token store the
// restarted daemon reloaded) and resumes renewing, so the session-liveness lease
// never lapses and liveness never reclaims a still-running session's boundary.
//
// Driven black-box-style over the socket (mint_token / attach / lease.* are public
// RPCs), with a real Build -> Close -> Build on the SAME ledger file = a real
// daemon restart, and a short lease TTL so the controls run fast.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/daemon"
)

func TestSessionReattachSurvivesDaemonRestart(t *testing.T) {
	sock := shortSock(t)
	ledger := filepath.Join(t.TempDir(), "ledger.db") // a FILE: durable across restart
	repo := t.TempDir()

	build := func() (*daemon.Daemon, func() error) {
		t.Helper()
		d, closeLedger, err := Build(Config{SocketPath: sock, LedgerPath: ledger, RepoRoot: repo})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if err := d.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		go func() { _ = d.Serve() }()
		return d, closeLedger
	}

	// D1: a launcher-like session mints a token and acquires the session-liveness lease.
	d1, close1 := build()
	a := dialc(t, sock)
	var who struct {
		Session string `json:"session"`
	}
	resultOf(t, a.call(t, "session.whoami", nil), &who)
	idA := who.Session
	if idA == "" {
		t.Fatal("whoami returned an empty session")
	}
	var mint struct {
		Token       string `json:"token"`
		LivenessKey string `json:"liveness_key"`
	}
	resultOf(t, a.call(t, "session.mint_token", map[string]any{}), &mint)
	var acq struct {
		Granted bool `json:"granted"`
	}
	resultOf(t, a.call(t, "lease.acquire", map[string]any{"key": mint.LivenessKey, "ttl_ms": 3000}), &acq)
	if !acq.Granted {
		t.Fatal("acquire of the session-liveness lease must grant")
	}
	a.close()

	// DAEMON RESTART: close D1 (its in-memory session registry + token map are lost),
	// rebuild D2 on the SAME ledger (the durable token + durable lease persist).
	_ = d1.Close()
	_ = close1()
	d2, close2 := build()
	defer func() { _ = d2.Close(); _ = close2() }()

	// THE #4 MECHANISM: a NEW connection re-attaches via the token, rebinding to the
	// ORIGINAL identity even though the new daemon never minted it in memory.
	b := dialc(t, sock)
	defer b.close()
	var att struct {
		Session string `json:"session"`
	}
	resultOf(t, b.call(t, "session.attach", map[string]any{"token": mint.Token}), &att)
	if att.Session != idA {
		t.Fatalf("re-attach after restart must rebind to the original identity %q; got %q", idA, att.Session)
	}
	// Renew now succeeds as the restored holder — the lease never lapses, so liveness
	// would not reclaim this still-live session.
	var rn struct {
		OK bool `json:"ok"`
	}
	resultOf(t, b.call(t, "lease.renew", map[string]any{"key": mint.LivenessKey, "ttl_ms": 3000}), &rn)
	if !rn.OK {
		t.Fatal("renew after re-attach must succeed (the original holder was restored)")
	}

	// CONTROL A: durability did NOT weaken auth — a garbage token is still refused.
	if resp := b.call(t, "session.attach", map[string]any{"token": "not-a-real-token"}); resp.Error == nil {
		t.Fatal("a garbage token must be refused after a restart too")
	}

	// CONTROL B: the durable token alone is NOT enough — the liveness gate still holds.
	// A session whose lease is allowed to EXPIRE cannot be attached to, even though its
	// token is durably stored.
	c := dialc(t, sock)
	var whoC struct {
		Session string `json:"session"`
	}
	resultOf(t, c.call(t, "session.whoami", nil), &whoC)
	var mintC struct {
		Token       string `json:"token"`
		LivenessKey string `json:"liveness_key"`
	}
	resultOf(t, c.call(t, "session.mint_token", map[string]any{}), &mintC)
	var acqC struct {
		Granted bool `json:"granted"`
	}
	resultOf(t, c.call(t, "lease.acquire", map[string]any{"key": mintC.LivenessKey, "ttl_ms": 200}), &acqC)
	if !acqC.Granted {
		t.Fatal("control: acquire C must grant")
	}
	c.close()                          // stop renewing C
	time.Sleep(400 * time.Millisecond) // let C's lease EXPIRE

	d := dialc(t, sock)
	defer d.close()
	if resp := d.call(t, "session.attach", map[string]any{"token": mintC.Token}); resp.Error == nil {
		t.Fatal("control: a token whose session-liveness lease EXPIRED must NOT attach (durable token does not bypass the liveness gate)")
	}
}
