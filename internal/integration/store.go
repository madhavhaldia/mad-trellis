package integration

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite" // pure-Go, cgo-free driver; registers "sqlite"
)

// State is an integration request's review lifecycle state. The machine is
// linear with a fork at the verdict:
//
//	requested -> claimed -> {changes_requested | approved}
//
// approved is the durable success terminal. changes_requested is a SOFT
// terminal: the builder revises and re-REQUESTS, which resets the row back to
// requested (clearing the prior claimer + feedback). requested/claimed are the
// in-flight states an integrator drives.
type State string

const (
	StateRequested        State = "requested"
	StateClaimed          State = "claimed"
	StateChangesRequested State = "changes_requested"
	StateApproved         State = "approved"
	// StateWithdrawn is a TERMINAL state for a request a builder (or an operator)
	// cancelled while it was still in-flight (requested/claimed). It clears the row
	// from the integrator's queue without an approve/reject verdict ever landing.
	StateWithdrawn State = "withdrawn"
)

// Record is one integration request's durable row. It is keyed by Branch — the
// boundary branch IS the identity (ID == Branch), so a builder re-requesting the
// same branch UPSERTs the one row rather than spawning duplicates. This plane is
// PURE records + state machine: no git ref, no merge happens here. Merge holds
// the commit OID the (external) integrator already produced and passes on
// approve, recorded only so the builder's status poll can see it.
type Record struct {
	Branch    string // == ID
	Title     string
	Holder    string // the session that REQUESTED (the builder; Inv 4)
	Claimer   string // the session that CLAIMED (the integrator); "" until claimed
	State     State
	Feedback  string // populated on reject (changes_requested)
	Merge     string // the merge commit OID, populated on approve
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ID returns the record's identity, which is its Branch (the two are the same
// key — one row per branch).
func (r Record) ID() string { return r.Branch }

// Clock is the injected time source (no scattered time.Now() — deterministic,
// testable), mirroring the integrator store.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// store is the integration plane's OWN durable state — a sibling SQLite ledger
// to the integrator's, never shared. Single-writer discipline (one connection)
// gives atomic CAS transitions without SQLITE_BUSY, mirroring the lease ledger
// and the integrator store.
type store struct {
	db    *sql.DB
	clock Clock
}

func openStore(path string, clock Clock) (*store, error) {
	if clock == nil {
		clock = systemClock{}
	}
	dsn := path
	if dsn == "" {
		dsn = ":memory:"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &store{db: db, clock: clock}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *store) close() error { return s.db.Close() }

func (s *store) migrate() error {
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS integration_requests (
  branch     TEXT PRIMARY KEY,
  title      TEXT    NOT NULL DEFAULT '',
  holder     TEXT    NOT NULL DEFAULT '',
  claimer    TEXT    NOT NULL DEFAULT '',
  state      TEXT    NOT NULL,
  feedback   TEXT    NOT NULL DEFAULT '',
  merge_oid  TEXT    NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);`); err != nil {
		return err
	}
	return s.migrateEvents()
}

// upsertRequest is the builder's request/re-request path. It always lands the
// row in `requested` with the requesting session as holder and a CLEARED claimer
// + feedback (a re-request after revising must not carry a prior integrator's
// claim or stale rejection notes). created_at is preserved on an existing row;
// title is preserved when the caller passes an empty one. Idempotent UPSERT keyed
// by branch — one row per branch, never a duplicate.
func (s *store) upsertRequest(branch, title, holder string) (Record, error) {
	now := s.clock.Now()
	existing, ok, err := s.get(branch)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		_, err = s.db.Exec(
			`INSERT INTO integration_requests (branch, title, holder, claimer, state, feedback, merge_oid, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			branch, title, holder, "", string(StateRequested), "", "", now.UnixNano(), now.UnixNano())
		if err != nil {
			return Record{}, err
		}
		return Record{Branch: branch, Title: title, Holder: holder, State: StateRequested, CreatedAt: now, UpdatedAt: now}, nil
	}
	newTitle := title
	if newTitle == "" {
		newTitle = existing.Title
	}
	_, err = s.db.Exec(
		`UPDATE integration_requests SET title = ?, holder = ?, claimer = '', state = ?, feedback = '', merge_oid = '', updated_at = ? WHERE branch = ?`,
		newTitle, holder, string(StateRequested), now.UnixNano(), branch)
	if err != nil {
		return Record{}, err
	}
	return Record{Branch: branch, Title: newTitle, Holder: holder, State: StateRequested, CreatedAt: existing.CreatedAt, UpdatedAt: now}, nil
}

// claim atomically transitions branch from requested -> claimed and records the
// claiming session, in ONE statement. It returns ok=false (no error) when the row
// is not in `requested` — so a second integrator claiming the same branch is a
// safe no-op, never a corruption. The state predicate IS the guard.
func (s *store) claim(branch, claimer string) (bool, error) {
	now := s.clock.Now()
	res, err := s.db.Exec(
		`UPDATE integration_requests SET state = ?, claimer = ?, updated_at = ? WHERE branch = ? AND state = ?`,
		string(StateClaimed), claimer, now.UnixNano(), branch, string(StateRequested))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// reject atomically transitions branch from claimed -> changes_requested and
// stores the feedback, in ONE statement. ok=false (no error) when not in
// `claimed`.
func (s *store) reject(branch, feedback string) (bool, error) {
	now := s.clock.Now()
	res, err := s.db.Exec(
		`UPDATE integration_requests SET state = ?, feedback = ?, updated_at = ? WHERE branch = ? AND state = ?`,
		string(StateChangesRequested), feedback, now.UnixNano(), branch, string(StateClaimed))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// approve atomically transitions branch from claimed -> approved and stores the
// merge commit OID the caller already produced, in ONE statement. ok=false (no
// error) when not in `claimed`.
func (s *store) approve(branch, merge string) (bool, error) {
	now := s.clock.Now()
	res, err := s.db.Exec(
		`UPDATE integration_requests SET state = ?, merge_oid = ?, updated_at = ? WHERE branch = ? AND state = ?`,
		string(StateApproved), merge, now.UnixNano(), branch, string(StateClaimed))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *store) get(branch string) (Record, bool, error) {
	var (
		rec              Record
		state            string
		created, updated int64
	)
	err := s.db.QueryRow(
		`SELECT branch, title, holder, claimer, state, feedback, merge_oid, created_at, updated_at FROM integration_requests WHERE branch = ?`, branch).
		Scan(&rec.Branch, &rec.Title, &rec.Holder, &rec.Claimer, &state, &rec.Feedback, &rec.Merge, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	rec.State = State(state)
	rec.CreatedAt = time.Unix(0, created)
	rec.UpdatedAt = time.Unix(0, updated)
	return rec, true, nil
}

// pending returns every row still in `requested` (the integrator's work queue),
// oldest first.
func (s *store) pending() ([]Record, error) {
	return s.query(`WHERE state = ? ORDER BY created_at`, string(StateRequested))
}

// cancel atomically withdraws a branch that is still IN-FLIGHT — requested OR
// claimed — to the terminal `withdrawn` state, in ONE statement. The state
// predicate IS the guard: ok=false (no error) when the row is absent or already
// in a TERMINAL/verdict state (approved/changes_requested/withdrawn), so a cancel
// can never clobber a landed verdict. It clears the row from the integrator's
// pending queue (a builder abandoning a branch, or an operator clearing a stale
// entry whose claimer crashed).
func (s *store) cancel(branch string) (bool, error) {
	now := s.clock.Now()
	res, err := s.db.Exec(
		`UPDATE integration_requests SET state = ?, updated_at = ? WHERE branch = ? AND state IN (?, ?)`,
		string(StateWithdrawn), now.UnixNano(), branch, string(StateRequested), string(StateClaimed))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// reclaimStaleClaims reverts every `claimed` row whose CLAIMER session is dead
// (isDead reports true) back to `requested`, clearing the dead claimer + any
// feedback, so a crashed integrator never strands a request in `claimed` forever
// — the builder's work re-enters the queue for a live integrator. Each revert is
// a per-row CAS still guarded by `state = claimed` (a row that left `claimed`
// between the read and the write is skipped, never clobbered). A claimer that is
// still ALIVE (isDead=false) is NEVER touched. Returns the count reverted.
func (s *store) reclaimStaleClaims(isDead func(session string) bool) (int, error) {
	recs, err := s.reclaimStaleClaimRecords(isDead)
	return len(recs), err
}

func (s *store) reclaimStaleClaimRecords(isDead func(session string) bool) ([]Record, error) {
	claimed, err := s.query(`WHERE state = ?`, string(StateClaimed))
	if err != nil {
		return nil, err
	}
	var reclaimed []Record
	for _, rec := range claimed {
		if !isDead(rec.Claimer) {
			continue // a live claimer is never reclaimed
		}
		now := s.clock.Now()
		res, err := s.db.Exec(
			`UPDATE integration_requests SET state = ?, claimer = '', feedback = '', updated_at = ? WHERE branch = ? AND state = ?`,
			string(StateRequested), now.UnixNano(), rec.Branch, string(StateClaimed))
		if err != nil {
			return reclaimed, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			rec.State = StateRequested
			rec.Claimer = ""
			rec.Feedback = ""
			rec.UpdatedAt = now
			reclaimed = append(reclaimed, rec)
		}
	}
	return reclaimed, nil
}

// gc deletes aged-out rows by `updated_at`, in TWO bounded DELETEs:
//
//   - TERMINAL rows (approved/withdrawn) older than terminalRetention — a landed
//     verdict has been observable by the builder for at least that long; the queue
//     should not carry decided rows forever.
//   - ABANDONED rows (requested/changes_requested) older than abandonedRetention — a
//     request nobody integrated, or a rejection the builder never revised, that has
//     gone cold. abandonedRetention is LONG by design: a `requested` row whose
//     builder merely EXITED is NORMAL (the builder requests, then exits for async
//     review), so this is a TTL on coldness, NOT a reclaim-on-builder-death.
//
// A `claimed` row is NEVER deleted here — it is in-flight under review, and the
// liveness stale-claim sweep (reclaimStaleClaims) already reverts it if its claimer
// died. Returns the total number of rows deleted. now is injected (the store's
// clock) so a test can drive coldness deterministically.
func (s *store) gc(now time.Time, terminalRetention, abandonedRetention time.Duration) (int, error) {
	total := 0
	termCutoff := now.Add(-terminalRetention).UnixNano()
	res, err := s.db.Exec(
		`DELETE FROM integration_requests WHERE state IN (?, ?) AND updated_at < ?`,
		string(StateApproved), string(StateWithdrawn), termCutoff)
	if err != nil {
		return total, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		total += int(n)
	}
	abandCutoff := now.Add(-abandonedRetention).UnixNano()
	res, err = s.db.Exec(
		`DELETE FROM integration_requests WHERE state IN (?, ?) AND updated_at < ?`,
		string(StateRequested), string(StateChangesRequested), abandCutoff)
	if err != nil {
		return total, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		total += int(n)
	}
	if err := s.gcEvents(now); err != nil {
		return total, err
	}
	return total, nil
}

// list returns EVERY row in any state, newest-updated first. Read-only — the
// watch surface consumes it to render the whole queue.
func (s *store) list() ([]Record, error) {
	return s.query(`ORDER BY updated_at DESC`)
}

func (s *store) query(where string, args ...any) ([]Record, error) {
	rows, err := s.db.Query(
		`SELECT branch, title, holder, claimer, state, feedback, merge_oid, created_at, updated_at FROM integration_requests `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var (
			rec              Record
			state            string
			created, updated int64
		)
		if err := rows.Scan(&rec.Branch, &rec.Title, &rec.Holder, &rec.Claimer, &state, &rec.Feedback, &rec.Merge, &created, &updated); err != nil {
			return nil, err
		}
		rec.State = State(state)
		rec.CreatedAt = time.Unix(0, created)
		rec.UpdatedAt = time.Unix(0, updated)
		out = append(out, rec)
	}
	return out, rows.Err()
}
