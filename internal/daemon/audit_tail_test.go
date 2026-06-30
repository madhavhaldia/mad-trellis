package daemon

// audit.tail — the ONLY new daemon surface for project 9a. These tests assert:
//   - newest-first ordering and limit honoring through the RPC,
//   - empty (NOT error) when no reader is configured,
//   - empty (NOT error) when the reader has no records,
//   - ZERO mutation (a tail call never appends; AuditCount-equivalent unchanged).
// The builtin is registered unconditionally; the reader is optional (Options).

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// memReader is an in-memory AuditReader+AuditSink for daemon-level tests (the
// lease ledger's real Tail is covered in internal/lease). Tail is newest-first.
type memReader struct {
	mu      sync.Mutex
	recs    []AuditRecord
	appends int
	tails   int
}

func (m *memReader) Append(r AuditRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appends++
	m.recs = append(m.recs, r)
	return nil
}

func (m *memReader) Tail(limit int) ([]AuditRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tails++
	// Copy + sort newest-first by timestamp (stable on equal ts via index).
	cp := make([]AuditRecord, len(m.recs))
	copy(cp, m.recs)
	sort.SliceStable(cp, func(i, j int) bool {
		return cp[i].Timestamp.After(cp[j].Timestamp)
	})
	if limit > 0 && limit < len(cp) {
		cp = cp[:limit]
	}
	return cp, nil
}

func (m *memReader) appendCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appends
}

type tailResult struct {
	Records []struct {
		TimestampMs     int64  `json:"timestamp_ms"`
		Session         string `json:"session"`
		DecisionProject string `json:"decision_project"`
		DecisionKind    string `json:"decision_kind"`
	} `json:"records"`
}

// startDaemonWith starts a daemon configured with both the sink and the reader.
func startDaemonWith(t *testing.T, r *memReader) (*Daemon, string) {
	t.Helper()
	path := tmpSock(t)
	d := New(Options{SocketPath: path, Audit: r, AuditReader: r})
	if err := d.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	go func() { _ = d.Serve() }()
	t.Cleanup(func() { _ = d.Close() })
	return d, path
}

func TestAuditTailNewestFirstAndLimit(t *testing.T) {
	r := &memReader{}
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		_ = r.Append(AuditRecord{
			Timestamp:       base.Add(time.Duration(i) * time.Minute),
			Session:         SessionID("s-x"),
			DecisionProject: "proj",
			DecisionKind:    "kind",
			// kind suffix encodes insertion order so we can assert ordering.
		})
		r.recs[i].DecisionKind = []string{"k0", "k1", "k2", "k3", "k4"}[i]
	}
	_, path := startDaemonWith(t, r)
	c := dialClient(t, path)
	defer c.close()

	var full tailResult
	mustResult(t, c.call(t, "audit.tail", map[string]int{"limit": 10}), &full)
	if len(full.Records) != 5 {
		t.Fatalf("want all 5 records, got %d", len(full.Records))
	}
	// Newest-first: k4 (latest timestamp) must be first, k0 last.
	if full.Records[0].DecisionKind != "k4" || full.Records[4].DecisionKind != "k0" {
		t.Fatalf("records must be newest-first; got first=%q last=%q",
			full.Records[0].DecisionKind, full.Records[4].DecisionKind)
	}
	// Strictly decreasing timestamps.
	for i := 1; i < len(full.Records); i++ {
		if full.Records[i].TimestampMs > full.Records[i-1].TimestampMs {
			t.Fatalf("timestamps must be non-increasing (newest-first); got %d after %d",
				full.Records[i].TimestampMs, full.Records[i-1].TimestampMs)
		}
	}

	// limit honored: top 2 newest.
	var limited tailResult
	mustResult(t, c.call(t, "audit.tail", map[string]int{"limit": 2}), &limited)
	if len(limited.Records) != 2 {
		t.Fatalf("limit=2 must yield 2 records, got %d", len(limited.Records))
	}
	if limited.Records[0].DecisionKind != "k4" || limited.Records[1].DecisionKind != "k3" {
		t.Fatalf("limit must take the NEWEST records; got %q,%q",
			limited.Records[0].DecisionKind, limited.Records[1].DecisionKind)
	}
}

func TestAuditTailEmptyWhenNoRecords(t *testing.T) {
	r := &memReader{}
	_, path := startDaemonWith(t, r)
	c := dialClient(t, path)
	defer c.close()
	var res tailResult
	resp := c.call(t, "audit.tail", map[string]int{"limit": 10})
	if resp.Error != nil {
		t.Fatalf("audit.tail with no records must NOT error; got %+v", resp.Error)
	}
	mustResult(t, resp, &res)
	if len(res.Records) != 0 {
		t.Fatalf("want empty records, got %d", len(res.Records))
	}
}

func TestAuditTailEmptyWhenNoReaderConfigured(t *testing.T) {
	// A minimally-composed daemon (no AuditReader) must still SERVE audit.tail and
	// return an empty list, not an error or method-not-found.
	_, path := startDaemon(t, nil) // no reader installed
	c := dialClient(t, path)
	defer c.close()
	resp := c.call(t, "audit.tail", map[string]int{"limit": 10})
	if resp.Error != nil {
		t.Fatalf("audit.tail with no reader must NOT error; got %+v", resp.Error)
	}
	var res tailResult
	mustResult(t, resp, &res)
	if len(res.Records) != 0 {
		t.Fatalf("no reader => empty records, got %d", len(res.Records))
	}
}

func TestAuditTailDefaultsLimitAndPerformsNoMutation(t *testing.T) {
	r := &memReader{}
	for i := 0; i < 3; i++ {
		_ = r.Append(AuditRecord{Timestamp: time.Now(), Session: "s", DecisionProject: "p", DecisionKind: "k"})
	}
	before := r.appendCount()
	_, path := startDaemonWith(t, r)
	c := dialClient(t, path)
	defer c.close()

	// limit<=0 must default sensibly (not error, not empty).
	var res tailResult
	mustResult(t, c.call(t, "audit.tail", map[string]int{"limit": 0}), &res)
	if len(res.Records) != 3 {
		t.Fatalf("limit<=0 must default and return all 3, got %d", len(res.Records))
	}
	// No params at all also defaults.
	var res2 tailResult
	mustResult(t, c.call(t, "audit.tail", nil), &res2)
	if len(res2.Records) != 3 {
		t.Fatalf("missing params must default; got %d", len(res2.Records))
	}

	// ZERO mutation: a tail call never appends.
	if got := r.appendCount(); got != before {
		t.Fatalf("audit.tail mutated the store: append count went %d -> %d", before, got)
	}
}

// TestNormalizeAuditLimit covers the daemon's limit normalization directly
// (default when non-positive, cap when oversized).
func TestNormalizeAuditLimit(t *testing.T) {
	if got := normalizeAuditLimit(0); got != auditTailDefaultLimit {
		t.Fatalf("limit 0 => default %d, got %d", auditTailDefaultLimit, got)
	}
	if got := normalizeAuditLimit(-5); got != auditTailDefaultLimit {
		t.Fatalf("negative limit => default %d, got %d", auditTailDefaultLimit, got)
	}
	if got := normalizeAuditLimit(10_000); got != auditTailMaxLimit {
		t.Fatalf("oversized limit => cap %d, got %d", auditTailMaxLimit, got)
	}
	if got := normalizeAuditLimit(42); got != 42 {
		t.Fatalf("in-range limit passes through; got %d", got)
	}
}
