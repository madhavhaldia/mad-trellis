package integrator

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite" // pure-Go, cgo-free driver; registers "sqlite"
)

// State is an integration's lifecycle state (docs/0004 card 6: explicit states
// received/validating/promoted/aborted). promoted and aborted are TERMINAL.
type State string

const (
	StateReceived   State = "received"
	StateValidating State = "validating"
	StatePromoted   State = "promoted"
	StateAborted    State = "aborted"
)

// Record is one integration's durable row. The git trunk ref is the authority
// for the atomic OUTCOME (trunk == MergeCommit ⟺ promoted); this record tracks
// intent + holder + cached state so the promote is idempotent and a daemon
// restart can re-detect in-flight integrations for liveness (project 8). Base is
// the trunk tip at submit time; MergeCommit is the intended promoted commit
// (written to the object store during validating, before any ref moves).
type Record struct {
	ID          string
	Branch      string
	Holder      string // the daemon session that submitted (Inv 4)
	Base        string // trunk tip captured at submit
	MergeCommit string // the intended/created merge commit (empty until validating)
	State       State
	GateOK      bool
	GateReason  string
	CreatedAt   time.Time
}

// Clock is the injected time source (no scattered time.Now() — deterministic,
// testable).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// store is the integrator's OWN durable state — separate from the lease ledger
// (the integrator never imports the lease store; the only cross-project store
// edge in mad-trellis is the audit table, written behind the daemon's method).
// Single-writer discipline (one connection) gives atomic CAS transitions without
// SQLITE_BUSY, mirroring the lease ledger.
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
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS integrations (
  id           TEXT PRIMARY KEY,
  branch       TEXT    NOT NULL,
  holder       TEXT    NOT NULL DEFAULT '',
  base         TEXT    NOT NULL DEFAULT '',
  merge_commit TEXT    NOT NULL DEFAULT '',
  state        TEXT    NOT NULL,
  gate_ok      INTEGER NOT NULL DEFAULT 0,
  gate_reason  TEXT    NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL
);`)
	return err
}

// insertReceived records a new integration in `received`. If the id already
// exists (a re-submit), it returns the existing record and existed=true without
// modifying it — the caller resumes from the durable state (idempotency).
func (s *store) insertReceived(id, branch, holder, base string) (rec Record, existed bool, err error) {
	if existing, ok, gerr := s.get(id); gerr != nil {
		return Record{}, false, gerr
	} else if ok {
		return existing, true, nil
	}
	now := s.clock.Now()
	_, err = s.db.Exec(
		`INSERT INTO integrations (id, branch, holder, base, state, created_at) VALUES (?,?,?,?,?,?)`,
		id, branch, holder, base, string(StateReceived), now.UnixNano())
	if err != nil {
		return Record{}, false, err
	}
	return Record{ID: id, Branch: branch, Holder: holder, Base: base, State: StateReceived, CreatedAt: now}, false, nil
}

// cas atomically transitions id from `from` to `to`, returning ok=false (no
// error) when the row is not in `from` — so a double transition is a safe no-op,
// not a corruption. The state predicate IS the guard (rows-affected decides).
func (s *store) cas(id string, from, to State) (bool, error) {
	res, err := s.db.Exec(`UPDATE integrations SET state = ? WHERE id = ? AND state = ?`,
		string(to), id, string(from))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// setValidating atomically moves received->validating AND records the base (the
// fresh trunk tip the merge commit was built on — and the update-ref CAS
// old-value), the intended merge commit, and the gate result in ONE statement,
// so a reader never sees `validating` without the base + merge commit it will
// promote against.
func (s *store) setValidating(id, base, mergeCommit string, gateOK bool, gateReason string) (bool, error) {
	ok := 0
	if gateOK {
		ok = 1
	}
	res, err := s.db.Exec(
		`UPDATE integrations SET state = ?, base = ?, merge_commit = ?, gate_ok = ?, gate_reason = ? WHERE id = ? AND state = ?`,
		string(StateValidating), base, mergeCommit, ok, gateReason, id, string(StateReceived))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *store) get(id string) (Record, bool, error) {
	var (
		rec   Record
		state string
		gate  int
		ts    int64
	)
	err := s.db.QueryRow(
		`SELECT id, branch, holder, base, merge_commit, state, gate_ok, gate_reason, created_at FROM integrations WHERE id = ?`, id).
		Scan(&rec.ID, &rec.Branch, &rec.Holder, &rec.Base, &rec.MergeCommit, &state, &gate, &rec.GateReason, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	rec.State = State(state)
	rec.GateOK = gate != 0
	rec.CreatedAt = time.Unix(0, ts)
	return rec, true, nil
}

// inFlight returns every non-terminal integration (received|validating) — the
// set liveness scans on a holder death / daemon restart to fire idempotent
// aborts.
func (s *store) inFlight() ([]Record, error) {
	return s.query(`WHERE state IN (?, ?) ORDER BY created_at`, string(StateReceived), string(StateValidating))
}

func (s *store) list() ([]Record, error) { return s.query(`ORDER BY created_at`) }

func (s *store) query(where string, args ...any) ([]Record, error) {
	rows, err := s.db.Query(
		`SELECT id, branch, holder, base, merge_commit, state, gate_ok, gate_reason, created_at FROM integrations `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var (
			rec   Record
			state string
			gate  int
			ts    int64
		)
		if err := rows.Scan(&rec.ID, &rec.Branch, &rec.Holder, &rec.Base, &rec.MergeCommit, &state, &gate, &rec.GateReason, &ts); err != nil {
			return nil, err
		}
		rec.State = State(state)
		rec.GateOK = gate != 0
		rec.CreatedAt = time.Unix(0, ts)
		out = append(out, rec)
	}
	return out, rows.Err()
}
