// Package lease implements project 2 (lease-ledger-mutex): the embedded-SQLite
// lease store and the deterministic, no-LLM mutual-exclusion lock service. It
// is the single source of truth for who holds exclusive access to any
// convergent or gated resource.
//
// Owns (docs/0003 clause map): 2(a) (atomic CAS lock path), 3-durable (durable
// store + TTL + renew-only-by-live-holder + the reclaim-if-expired CAS), and
// its local 2(b) slice (keys are OPAQUE — never parsed/classified). It does NOT
// decide WHAT needs a lease (the classifier's job), does NOT detect dead
// holders or own reclaim timing (liveness's job — it only provides the
// primitive), and never becomes a second arbiter.
//
// Correctness rule (docs/0004 card 2): every acquire/renew/release is a SINGLE
// atomic conditional SQL statement whose WHERE clause IS the lock predicate —
// never SELECT-then-write. The grant decision is read from rows-affected.
package lease

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite" // pure-Go, cgo-free driver; registers "sqlite"
)

// Clock is the injected time source (no scattered time.Now() on the lock path,
// so TTL/expiry is deterministic and testable — Inv 2(b)).
type Clock interface{ Now() time.Time }

// SystemClock is the production wall-clock.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now() }

// Ledger is the durable lease store. All access is serialized to a single
// connection (single-writer discipline, Inv 2(a)/5).
type Ledger struct {
	db    *sql.DB
	clock Clock
}

// AcquireResult is the outcome of an Acquire.
type AcquireResult struct {
	Granted   bool
	Holder    string
	ExpiresAt time.Time
	Fence     int64
	// Head is the session at the front of the intent queue when a queue exists
	// (Wing 4 fair hand-off), or "" when the key has no live waiters. Ahead is the
	// count of live waiters strictly ahead of the caller (0 when the caller is the
	// head, or when there is no queue). Both are advisory HINTS for a refused
	// caller and never alter the grant decision (rows-affected is authoritative).
	Head  string
	Ahead int
}

// RenewResult is the outcome of a Renew.
type RenewResult struct {
	OK        bool
	ExpiresAt time.Time
}

// ReclaimResult is the outcome of a ReclaimIfExpired.
type ReclaimResult struct {
	Reclaimed   bool
	PriorHolder string
}

// LeaseInfo is the read-only view of a lease (for the watch surface, project 9a).
type LeaseInfo struct {
	Key       []byte
	Holder    string
	ExpiresAt time.Time
	Fence     int64
	Held      bool // holder present AND not expired (live)
}

// Open opens (creating if needed) the ledger at path. Pass "" / ":memory:" for
// an in-memory store. A nil clock defaults to SystemClock.
func Open(path string, clock Clock) (*Ledger, error) {
	if clock == nil {
		clock = SystemClock{}
	}
	dsn := path
	if dsn == "" {
		dsn = ":memory:"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Single-writer discipline: one connection serializes ALL access, giving
	// atomic CAS without SQLITE_BUSY (Inv 2(a)/5).
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	l := &Ledger{db: db, clock: clock}
	if err := l.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return l, nil
}

// Close closes the ledger.
func (l *Ledger) Close() error { return l.db.Close() }

func (l *Ledger) migrate() error {
	_, err := l.db.Exec(`
CREATE TABLE IF NOT EXISTS leases (
  key         BLOB PRIMARY KEY,
  holder      TEXT    NOT NULL DEFAULT '',
  acquired_at INTEGER NOT NULL DEFAULT 0,
  expires_at  INTEGER NOT NULL DEFAULT 0,
  fence       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS audit_log (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  timestamp        INTEGER NOT NULL,
  session          TEXT    NOT NULL,
  decision_project TEXT    NOT NULL,
  decision_kind    TEXT    NOT NULL,
  payload          BLOB
);
CREATE TABLE IF NOT EXISTS session_tokens (
  token_hash TEXT PRIMARY KEY,
  session_id TEXT    NOT NULL,
  created_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS lease_waiters (
  key         BLOB    NOT NULL,
  session     TEXT    NOT NULL,
  enqueued_at INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  PRIMARY KEY (key, session)
);`)
	return err
}

// --- Durable session-token store (T2 + P0 #4) ----------------------------------
// The capability-token binding (token HASH -> sessionID) is held HERE so
// session.attach survives a daemon restart: after a restart the launcher re-attaches
// via its token instead of the restart orphaning a still-live session. Only the
// HASH is stored (never the raw bearer secret). The session package reaches these
// through an interface (it never imports lease), wired by compose.

// PutSessionToken records a token-hash -> sessionID binding (createdMs = unix-ms).
// An existing hash is overwritten (idempotent re-mint).
func (l *Ledger) PutSessionToken(tokenHash, sessionID string, createdMs int64) error {
	_, err := l.db.Exec(
		`INSERT INTO session_tokens (token_hash, session_id, created_ms) VALUES (?, ?, ?)
		 ON CONFLICT(token_hash) DO UPDATE SET session_id = excluded.session_id, created_ms = excluded.created_ms`,
		tokenHash, sessionID, createdMs)
	return err
}

// GetSessionToken returns the sessionID bound to a token hash (ok=false if absent).
func (l *Ledger) GetSessionToken(tokenHash string) (string, bool, error) {
	var id string
	err := l.db.QueryRow(`SELECT session_id FROM session_tokens WHERE token_hash = ?`, tokenHash).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// DeleteSessionToken removes a token-hash binding (idempotent).
func (l *Ledger) DeleteSessionToken(tokenHash string) error {
	_, err := l.db.Exec(`DELETE FROM session_tokens WHERE token_hash = ?`, tokenHash)
	return err
}

// ListSessionTokens returns every token binding (hash, sessionID, createdMs) for
// the session store's prune pass.
func (l *Ledger) ListSessionTokens() ([]SessionTokenRow, error) {
	rows, err := l.db.Query(`SELECT token_hash, session_id, created_ms FROM session_tokens`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionTokenRow
	for rows.Next() {
		var r SessionTokenRow
		if err := rows.Scan(&r.TokenHash, &r.SessionID, &r.CreatedMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SessionTokenRow is one durable session-token binding.
type SessionTokenRow struct {
	TokenHash string
	SessionID string
	CreatedMs int64
}

// Acquire grants the lease iff it is free OR expired (at the injected clock),
// in a SINGLE atomic upsert whose WHERE clause is the lock predicate. The grant
// is decided by rows-affected, never by a read-then-write window. Fail-fast on
// a live conflict (no queue, no wait — the v1 CAS-fail-fast wait policy).
func (l *Ledger) Acquire(key []byte, holder string, ttl time.Duration) (AcquireResult, error) {
	if holder == "" {
		return AcquireResult{}, errors.New("lease: holder token required")
	}
	now := l.clock.Now()
	nowN := now.UnixNano()
	expN := now.Add(ttl).UnixNano()

	// Wing 4 (same-path contention, R8): a dead waiter's row expires by TTL — self-
	// contained in the lease store, no liveness coupling. Prune lazily on every
	// acquire so a stale queue can never starve a free key.
	if err := l.pruneWaiters(nowN); err != nil {
		return AcquireResult{}, err
	}
	head, ahead, hasQueue, err := l.headWaiter(key, holder, nowN)
	if err != nil {
		return AcquireResult{}, err
	}
	// FAIR HAND-OFF / ANTI-BARGING: when an intent queue exists, only the head-of-
	// queue waiter may take the key — a non-head caller (or one not enqueued) is
	// REFUSED even if the key is free. This activates ONLY when a queue exists; with
	// NO live waiters the path below is byte-for-byte the original CAS (the common
	// path every existing lease test + the deterministic-lock-path conformance rely
	// on stays unchanged).
	if hasQueue && holder != head {
		info, _, ierr := l.Inspect(key)
		if ierr != nil {
			return AcquireResult{}, ierr
		}
		return AcquireResult{Granted: false, Holder: info.Holder, ExpiresAt: info.ExpiresAt, Fence: info.Fence, Head: head, Ahead: ahead}, nil
	}

	res, err := l.db.Exec(`
INSERT INTO leases (key, holder, acquired_at, expires_at, fence)
VALUES (?, ?, ?, ?, 1)
ON CONFLICT(key) DO UPDATE SET
  holder      = excluded.holder,
  acquired_at = excluded.acquired_at,
  expires_at  = excluded.expires_at,
  fence       = leases.fence + 1
WHERE leases.holder = '' OR leases.expires_at <= ?`,
		key, holder, nowN, expN, nowN)
	if err != nil {
		return AcquireResult{}, err
	}
	n, _ := res.RowsAffected()
	info, _, err := l.Inspect(key)
	if err != nil {
		return AcquireResult{}, err
	}
	if n == 1 {
		// Hand-off complete: the head waiter took the key, so retire its waiter row.
		if hasQueue {
			if _, derr := l.db.Exec(`DELETE FROM lease_waiters WHERE key = ? AND session = ?`, key, holder); derr != nil {
				return AcquireResult{}, derr
			}
		}
		return AcquireResult{Granted: true, Holder: holder, ExpiresAt: time.Unix(0, expN), Fence: info.Fence}, nil
	}
	return AcquireResult{Granted: false, Holder: info.Holder, ExpiresAt: info.ExpiresAt, Fence: info.Fence, Head: head, Ahead: ahead}, nil
}

// Renew extends the lease ONLY when the caller is the current holder AND the
// lease is still live (not expired). A non-holder or expired renew fails.
func (l *Ledger) Renew(key []byte, holder string, ttl time.Duration) (RenewResult, error) {
	now := l.clock.Now()
	expN := now.Add(ttl).UnixNano()
	res, err := l.db.Exec(
		`UPDATE leases SET expires_at = ? WHERE key = ? AND holder = ? AND holder != '' AND expires_at > ?`,
		expN, key, holder, now.UnixNano())
	if err != nil {
		return RenewResult{}, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return RenewResult{OK: true, ExpiresAt: time.Unix(0, expN)}, nil
	}
	return RenewResult{OK: false}, nil
}

// Release frees the lease ONLY when the caller is the current holder. It is
// idempotent: a second release (or a non-holder release) returns ok=false
// without error.
func (l *Ledger) Release(key []byte, holder string) (bool, error) {
	if holder == "" {
		return false, nil
	}
	res, err := l.db.Exec(
		`UPDATE leases SET holder = '', expires_at = 0 WHERE key = ? AND holder = ? AND holder != ''`,
		key, holder)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReclaimIfExpired frees the lease iff it is held AND expired at the injected
// clock — the deterministic, idempotent, SOLE mutation path for an expired
// lease. It evaluates EXPIRY only, never holder liveness (that is liveness-
// recovery's trigger; this is the primitive it invokes). The free decision is
// the conditional UPDATE's rows-affected; the prior holder is read for
// reporting only. Bumps the fence so a stale prior holder cannot re-act.
func (l *Ledger) ReclaimIfExpired(key []byte) (ReclaimResult, error) {
	nowN := l.clock.Now().UnixNano()
	tx, err := l.db.Begin()
	if err != nil {
		return ReclaimResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var prior string
	switch err := tx.QueryRow(`SELECT holder FROM leases WHERE key = ?`, key).Scan(&prior); {
	case errors.Is(err, sql.ErrNoRows):
		return ReclaimResult{}, nil // no such lease — nothing to reclaim
	case err != nil:
		return ReclaimResult{}, err
	}
	res, err := tx.Exec(
		`UPDATE leases SET holder = '', expires_at = 0, fence = fence + 1 WHERE key = ? AND holder != '' AND expires_at <= ?`,
		key, nowN)
	if err != nil {
		return ReclaimResult{}, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return ReclaimResult{}, err
	}
	if n == 1 {
		return ReclaimResult{Reclaimed: true, PriorHolder: prior}, nil
	}
	return ReclaimResult{Reclaimed: false}, nil
}

// Inspect returns the read-only view of a lease. ok=false when the key has no
// row. Keys are OPAQUE — Inspect never parses key content.
func (l *Ledger) Inspect(key []byte) (LeaseInfo, bool, error) {
	var holder string
	var exp, fence int64
	err := l.db.QueryRow(`SELECT holder, expires_at, fence FROM leases WHERE key = ?`, key).
		Scan(&holder, &exp, &fence)
	if errors.Is(err, sql.ErrNoRows) {
		return LeaseInfo{Key: key}, false, nil
	}
	if err != nil {
		return LeaseInfo{}, false, err
	}
	held := holder != "" && exp > l.clock.Now().UnixNano()
	return LeaseInfo{Key: key, Holder: holder, ExpiresAt: time.Unix(0, exp), Fence: fence, Held: held}, true, nil
}

// ListHolders returns all currently-held (live) leases — read-only, for the
// watch surface (project 9a).
func (l *Ledger) ListHolders() ([]LeaseInfo, error) {
	return l.listWhere(`holder != '' AND expires_at > ?`)
}

// ExpiredLeases returns held leases whose TTL has elapsed at the injected clock —
// the deterministic DEATH SIGNAL liveness-recovery (project 8) reads. It is
// READ-ONLY: liveness then invokes ReclaimIfExpired (the actual CAS); this never
// frees anything. A still-live (renewed) holder is never returned, so a detector
// built on it cannot trigger against a slow-but-live holder (the no-false-
// positive layer above the ledger's own ReclaimIfExpired guard).
func (l *Ledger) ExpiredLeases() ([]LeaseInfo, error) {
	return l.listWhere(`holder != '' AND expires_at <= ?`)
}

// --- Wing 4: same-path contention (intent queue + fair hand-off, R8) ----------
//
// A HEADLESS fair queue: when a convergent/path key is held, a waiter ENQUEUEs an
// intent and discovers its turn by POLLING (no push channel). The ledger gives
// queue ORDER (FIFO by enqueued_at); Acquire grants a free key ONLY to the head of
// that queue (anti-barging). It is self-contained — a dead waiter's row expires by
// TTL (no liveness coupling), pruned lazily on every enqueue/queue/acquire read.

// DefaultWaiterTTL is the lifetime of an intent-queue row when the caller does not
// pin one: a waiter that stops heartbeating (re-enqueuing) is pruned after this, so
// a crashed waiter can never permanently hold up the queue.
const DefaultWaiterTTL = 90 * time.Second

// EnqueueResult is the outcome of an Enqueue: the caller's 0-based queue position
// (rank by enqueued_at among live waiters) and the count of waiters ahead (= the
// same value; both are returned so the wire shape is explicit).
type EnqueueResult struct {
	Position int
	Ahead    int
}

// QueueWaiter is one live waiter in an intent queue's ordered snapshot.
type QueueWaiter struct {
	Session  string
	Position int
}

// QueueSnapshot is the read-only view of a key's contention state: whether it is
// HELD (live holder), the holder identity, and the ordered live waiters.
type QueueSnapshot struct {
	Held    bool
	Holder  string
	Waiters []QueueWaiter
}

// Enqueue registers (or refreshes) the caller's intent to acquire key. It is
// IDEMPOTENT per (key, session): a re-enqueue PRESERVES the original enqueued_at
// (so a waiter never loses its FIFO position by heartbeating) and only refreshes
// the TTL. Returns the caller's 0-based position among live waiters. ttl<=0 uses
// DefaultWaiterTTL. Prunes dead waiters first (self-contained TTL).
func (l *Ledger) Enqueue(key []byte, session string, ttl time.Duration) (EnqueueResult, error) {
	if session == "" {
		return EnqueueResult{}, errors.New("lease: session required to enqueue")
	}
	if ttl <= 0 {
		ttl = DefaultWaiterTTL
	}
	now := l.clock.Now()
	nowN := now.UnixNano()
	if err := l.pruneWaiters(nowN); err != nil {
		return EnqueueResult{}, err
	}
	expN := now.Add(ttl).UnixNano()
	// PRESERVE enqueued_at on conflict (stable FIFO order); refresh only expires_at
	// (the TTL heartbeat). enqueued_at orders the queue; expires_at ages out a dead
	// waiter independently, so a live waiter can heartbeat without losing its place.
	if _, err := l.db.Exec(`
INSERT INTO lease_waiters (key, session, enqueued_at, expires_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(key, session) DO UPDATE SET expires_at = excluded.expires_at`,
		key, session, nowN, expN); err != nil {
		return EnqueueResult{}, err
	}
	_, ahead, _, err := l.headWaiter(key, session, nowN)
	if err != nil {
		return EnqueueResult{}, err
	}
	return EnqueueResult{Position: ahead, Ahead: ahead}, nil
}

// Dequeue removes the caller's intent for key. Idempotent: ok=false when the caller
// had no live (or any) waiter row, without error.
func (l *Ledger) Dequeue(key []byte, session string) (bool, error) {
	if session == "" {
		return false, nil
	}
	res, err := l.db.Exec(`DELETE FROM lease_waiters WHERE key = ? AND session = ?`, key, session)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Queue returns the read-only contention snapshot for key: the live holder (if
// any) and the ordered live waiters. Prunes dead waiters first so the snapshot
// reflects only live intents.
func (l *Ledger) Queue(key []byte) (QueueSnapshot, error) {
	nowN := l.clock.Now().UnixNano()
	if err := l.pruneWaiters(nowN); err != nil {
		return QueueSnapshot{}, err
	}
	info, _, err := l.Inspect(key)
	if err != nil {
		return QueueSnapshot{}, err
	}
	snap := QueueSnapshot{Held: info.Held}
	if info.Held {
		snap.Holder = info.Holder
	}
	waiters, err := l.liveWaiters(key, nowN)
	if err != nil {
		return QueueSnapshot{}, err
	}
	for i, w := range waiters {
		snap.Waiters = append(snap.Waiters, QueueWaiter{Session: w, Position: i})
	}
	return snap, nil
}

// pruneWaiters deletes every waiter row whose TTL has elapsed at nowN. It is the
// SOLE expiry path for the intent queue (no liveness dependency) and is invoked
// lazily on every enqueue/queue/acquire read.
func (l *Ledger) pruneWaiters(nowN int64) error {
	_, err := l.db.Exec(`DELETE FROM lease_waiters WHERE expires_at <= ?`, nowN)
	return err
}

// liveWaiters returns the sessions of key's live (un-expired at nowN) waiters in
// FIFO order. The (session ASC) tiebreaker keeps ordering DETERMINISTIC when two
// rows share an enqueued_at (Inv 2(b) — no nondeterminism on the lock path).
func (l *Ledger) liveWaiters(key []byte, nowN int64) ([]string, error) {
	rows, err := l.db.Query(
		`SELECT session FROM lease_waiters WHERE key = ? AND expires_at > ? ORDER BY enqueued_at ASC, session ASC`,
		key, nowN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// headWaiter reports key's head-of-queue session, how many live waiters are
// strictly ahead of `session`, and whether ANY live waiter exists. When the caller
// is not enqueued, ahead is the full live-waiter count. hasQueue=false (no live
// waiters) is the signal Acquire uses to keep the original unqueued CAS path.
func (l *Ledger) headWaiter(key []byte, session string, nowN int64) (head string, ahead int, hasQueue bool, err error) {
	waiters, err := l.liveWaiters(key, nowN)
	if err != nil {
		return "", 0, false, err
	}
	if len(waiters) == 0 {
		return "", 0, false, nil
	}
	ahead = len(waiters) // default: caller not enqueued → behind everyone
	for i, w := range waiters {
		if w == session {
			ahead = i
			break
		}
	}
	return waiters[0], ahead, true, nil
}

func (l *Ledger) listWhere(pred string) ([]LeaseInfo, error) {
	nowN := l.clock.Now().UnixNano()
	live := pred == `holder != '' AND expires_at > ?`
	rows, err := l.db.Query(
		`SELECT key, holder, expires_at, fence FROM leases WHERE `+pred+` ORDER BY key`, nowN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LeaseInfo
	for rows.Next() {
		var key []byte
		var holder string
		var exp, fence int64
		if err := rows.Scan(&key, &holder, &exp, &fence); err != nil {
			return nil, err
		}
		out = append(out, LeaseInfo{Key: key, Holder: holder, ExpiresAt: time.Unix(0, exp), Fence: fence, Held: live})
	}
	return out, rows.Err()
}
