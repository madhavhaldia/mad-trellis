package integration

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

const (
	eventDefaultMax = 50
	eventMaxCap     = 200
	eventRetention  = 24 * time.Hour
)

// Event is a daemon-authored wake-up. It is deliberately small: the durable
// integration request row remains the truth, and event delivery carries no
// agent-authored payload.
type Event struct {
	ID        int64
	Audience  string
	Kind      string
	Branch    string
	CreatedAt time.Time
	Holder    string
}

func (s *store) migrateEvents() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  audience TEXT NOT NULL,
  kind TEXT NOT NULL,
  branch TEXT NOT NULL,
  created_at_ns INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS event_cursors (
  consumer TEXT PRIMARY KEY,
  last_id INTEGER NOT NULL
);`)
	return err
}

func (s *store) appendEvent(audience, kind, branch string) error {
	_, err := s.db.Exec(
		`INSERT INTO events (audience, kind, branch, created_at_ns) VALUES (?, ?, ?, ?)`,
		audience, kind, branch, s.clock.Now().UnixNano())
	return err
}

func (s *store) pollEvents(consumer, branch string, max int, authorized func(Event) bool) ([]Event, error) {
	max = normalizeEventMax(max)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var lastID int64
	err = tx.QueryRow(`SELECT last_id FROM event_cursors WHERE consumer = ?`, consumer).Scan(&lastID)
	if errors.Is(err, sql.ErrNoRows) {
		lastID = 0
	} else if err != nil {
		return nil, err
	}

	query := `SELECT e.id, e.audience, e.kind, e.branch, e.created_at_ns, r.holder
	          FROM events e
	          LEFT JOIN integration_requests r ON r.branch = e.branch
	          WHERE e.id > ?`
	args := []any{lastID}
	if branch != "" {
		query += ` AND e.branch = ?`
		args = append(args, branch)
	}
	query += ` ORDER BY e.id`

	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}

	out := make([]Event, 0, max)
	for rows.Next() {
		var (
			ev        Event
			createdNS int64
			holder    sql.NullString
		)
		if err := rows.Scan(&ev.ID, &ev.Audience, &ev.Kind, &ev.Branch, &createdNS, &holder); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ev.CreatedAt = time.Unix(0, createdNS)
		if holder.Valid {
			ev.Holder = holder.String
		}
		if !authorized(ev) {
			continue
		}
		out = append(out, ev)
		if len(out) == max {
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	if len(out) > 0 {
		lastID = out[len(out)-1].ID
		if _, err := tx.Exec(
			`INSERT INTO event_cursors (consumer, last_id) VALUES (?, ?)
			 ON CONFLICT(consumer) DO UPDATE SET last_id = excluded.last_id`,
			consumer, lastID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *store) gcEvents(now time.Time) error {
	_, err := s.db.Exec(`DELETE FROM events WHERE created_at_ns < ?`, now.Add(-eventRetention).UnixNano())
	return err
}

func normalizeEventMax(max int) int {
	if max <= 0 {
		return eventDefaultMax
	}
	if max > eventMaxCap {
		return eventMaxCap
	}
	return max
}

func branchAudience(branch string) string { return "branch:" + branch }

func branchFromAudience(audience string) (string, bool) {
	const prefix = "branch:"
	if !strings.HasPrefix(audience, prefix) {
		return "", false
	}
	return strings.TrimPrefix(audience, prefix), true
}
