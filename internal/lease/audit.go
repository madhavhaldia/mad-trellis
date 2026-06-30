package lease

import (
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
)

// AuditSink is the durable, append-only SQLite-backed implementation of
// daemon.AuditSink — the STORAGE behind the daemon's audit write-interface
// (docs/0003 L162). The build edge is lease->daemon (lease implements the
// daemon's interface); the daemon never imports lease, so the lease store is
// not a coupling hub. Append-only: INSERT only, no update or delete.
type AuditSink struct{ l *Ledger }

// AuditSink returns a durable audit sink backed by this ledger's database.
func (l *Ledger) AuditSink() *AuditSink { return &AuditSink{l: l} }

// Append implements daemon.AuditSink (append-only).
func (s *AuditSink) Append(rec daemon.AuditRecord) error {
	_, err := s.l.db.Exec(
		`INSERT INTO audit_log (timestamp, session, decision_project, decision_kind, payload) VALUES (?,?,?,?,?)`,
		rec.Timestamp.UnixNano(), string(rec.Session), rec.DecisionProject, rec.DecisionKind, []byte(rec.Payload))
	return err
}

// Tail implements daemon.AuditReader — the READ-ONLY companion to Append. It
// returns up to limit records NEWEST-FIRST via a single SELECT (no writes). The
// stored timestamp is a UnixNano int (see Append), mapped back to time.Time. The
// daemon normalizes limit (default/cap) before calling, but Tail defends itself
// too: a non-positive limit is clamped to 1 so the prepared LIMIT is always
// well-formed. Read-only SQL: SELECT only.
func (s *AuditSink) Tail(limit int) ([]daemon.AuditRecord, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.l.db.Query(
		`SELECT timestamp, session, decision_project, decision_kind, payload
		   FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]daemon.AuditRecord, 0, limit)
	for rows.Next() {
		var (
			tsNano  int64
			session string
			project string
			kind    string
			payload []byte
		)
		if err := rows.Scan(&tsNano, &session, &project, &kind, &payload); err != nil {
			return nil, err
		}
		rec := daemon.AuditRecord{
			Timestamp:       time.Unix(0, tsNano),
			Session:         daemon.SessionID(session),
			DecisionProject: project,
			DecisionKind:    kind,
		}
		if len(payload) > 0 {
			rec.Payload = append([]byte(nil), payload...)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// AuditCount returns the number of stored audit records (tests/diagnostics).
func (l *Ledger) AuditCount() (int, error) {
	var n int
	err := l.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n)
	return n, err
}
