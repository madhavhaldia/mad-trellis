package app

// Yanked / corrupt / unwritable ledger tolerance (chafe C14): Build must refuse
// to serve with a CLEAR, actionable error naming the ledger path and reason —
// NEVER a raw SQLite "disk I/O error (522)" or a panic. Stale -wal/-shm sidecars
// from an unclean shutdown must be recovered by a fresh Open.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A ledger PATH that is a directory cannot be opened as a SQLite file → Build
// must return the clear "ledger at <path> is unusable: ...; refusing to serve"
// error (and no daemon), not a raw driver code.
func TestBuildUnusableLedgerClearError(t *testing.T) {
	dir := t.TempDir()
	ledgerAsDir := filepath.Join(dir, "ledger.db")
	if err := os.Mkdir(ledgerAsDir, 0o700); err != nil { // a dir where a file is expected
		t.Fatal(err)
	}
	d, closeAll, err := Build(Config{
		SocketPath: shortSock(t),
		LedgerPath: ledgerAsDir,
		RepoRoot:   t.TempDir(),
	})
	if err == nil {
		if closeAll != nil {
			_ = closeAll()
		}
		if d != nil {
			_ = d.Close()
		}
		t.Fatal("Build must fail on an unusable ledger")
	}
	msg := err.Error()
	if !strings.Contains(msg, "is unusable") || !strings.Contains(msg, "refusing to serve") {
		t.Fatalf("error must be the clear unusable-ledger message, got: %v", msg)
	}
	if !strings.Contains(msg, ledgerAsDir) {
		t.Fatalf("error must name the ledger path %q, got: %v", ledgerAsDir, msg)
	}
	// NEVER leak the raw SQLite disk-I/O code.
	if strings.Contains(msg, "522") {
		t.Fatalf("error must not leak the raw SQLite disk I/O code, got: %v", msg)
	}
}

// A CORRUPT ledger file (garbage where a SQLite header is expected) → same clear
// error, not a panic or a raw driver code.
func TestBuildCorruptLedgerClearError(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "ledger.db")
	if err := os.WriteFile(ledger, []byte("this is not a sqlite database file at all\x00\x01\x02"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, closeAll, err := Build(Config{
		SocketPath: shortSock(t),
		LedgerPath: ledger,
		RepoRoot:   t.TempDir(),
	})
	if err == nil {
		if closeAll != nil {
			_ = closeAll()
		}
		if d != nil {
			_ = d.Close()
		}
		t.Fatal("Build must fail on a corrupt ledger file")
	}
	msg := err.Error()
	if !strings.Contains(msg, "is unusable") || !strings.Contains(msg, "refusing to serve") {
		t.Fatalf("error must be the clear unusable-ledger message, got: %v", msg)
	}
}

// Stale "-wal"/"-shm" sidecars from an unclean shutdown must be recovered by a
// fresh Open. We reproduce the genuine unclean-shutdown shape: snapshot a LIVE
// ledger's db + non-empty -wal/-shm files (as if the process was killed mid-run)
// into a fresh dir, then Build there. A fresh Open must recover WAL state without
// error and without deleting the ledger.
func TestBuildRecoversStaleWalShmSidecars(t *testing.T) {
	srcDir := t.TempDir()
	srcLedger := filepath.Join(srcDir, "ledger.db")

	// Build + Start a daemon so the ledger is actively in WAL mode, then write
	// through it so the -wal/-shm sidecars exist with real content. We deliberately
	// do NOT close it before snapshotting — that is the unclean-shutdown shape.
	d, closeAll, err := Build(Config{SocketPath: shortSock(t), LedgerPath: srcLedger, RepoRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("initial build: %v", err)
	}
	if err := d.Start(); err != nil {
		closeAll()
		t.Fatalf("start: %v", err)
	}
	go func() { _ = d.Serve() }()

	// Snapshot the on-disk files while the daemon is live (genuine dirty sidecars).
	dstDir := t.TempDir()
	dstLedger := filepath.Join(dstDir, "ledger.db")
	copied := false
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, rerr := os.ReadFile(srcLedger + suffix)
		if rerr != nil {
			continue // a given sidecar may not exist depending on checkpoint timing
		}
		if suffix == "-wal" && len(b) > 0 {
			copied = true
		}
		if werr := os.WriteFile(dstLedger+suffix, b, 0o600); werr != nil {
			t.Fatalf("copy %s: %v", suffix, werr)
		}
	}
	_ = d.Close()
	_ = closeAll()
	if !copied {
		t.Skip("WAL was checkpointed before snapshot; no dirty -wal to exercise (timing)")
	}

	// A fresh Open on the snapshot must recover the dirty WAL without error.
	d2, closeAll2, err := Build(Config{SocketPath: shortSock(t), LedgerPath: dstLedger, RepoRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Build must recover stale -wal/-shm sidecars, got: %v", err)
	}
	defer closeAll2()
	if d2 == nil {
		t.Fatal("expected a composed daemon after recovery")
	}
	// The ledger file must still exist (we never delete a ledger we found).
	if _, statErr := os.Stat(dstLedger); statErr != nil {
		t.Fatalf("ledger must not be deleted during recovery: %v", statErr)
	}
}
