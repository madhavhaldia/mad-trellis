package lease

// Tests for the READ-ONLY audit Tail (project 9a's audit.tail storage). The
// durable audit_log was write-only (Append + AuditCount); Tail is the symmetric
// read path. These assert newest-first ordering, limit honoring, empty-not-error
// on an empty table, faithful round-trip of the record fields, and that Tail
// performs ZERO writes (AuditCount unchanged by a tail).

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
)

func TestAuditSinkTailNewestFirstAndLimit(t *testing.T) {
	l := openLedger(t, nil)
	sink := l.AuditSink()

	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	for i := 0; i < 5; i++ {
		rec := daemon.AuditRecord{
			Timestamp:       base.Add(time.Duration(i) * time.Minute),
			Session:         daemon.SessionID("s-1-abc"),
			DecisionProject: "proj",
			DecisionKind:    []string{"k0", "k1", "k2", "k3", "k4"}[i],
			Payload:         json.RawMessage(`{"i":` + string(rune('0'+i)) + `}`),
		}
		if err := sink.Append(rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	all, err := sink.Tail(100)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("want 5 records, got %d", len(all))
	}
	// Newest-first: insertion order k0..k4, so Tail must yield k4..k0.
	want := []string{"k4", "k3", "k2", "k1", "k0"}
	for i, w := range want {
		if all[i].DecisionKind != w {
			t.Fatalf("record %d: want kind %q (newest-first), got %q", i, w, all[i].DecisionKind)
		}
	}
	// Field round-trip on the newest record.
	if all[0].DecisionProject != "proj" || string(all[0].Session) != "s-1-abc" {
		t.Fatalf("field round-trip failed: %+v", all[0])
	}
	// Timestamp round-trips through UnixNano storage.
	if !all[0].Timestamp.Equal(base.Add(4 * time.Minute)) {
		t.Fatalf("timestamp round-trip: want %v, got %v", base.Add(4*time.Minute), all[0].Timestamp)
	}
	// Payload preserved.
	if string(all[0].Payload) != `{"i":4}` {
		t.Fatalf("payload round-trip: got %s", all[0].Payload)
	}

	// limit honored: newest 2 only.
	top2, err := sink.Tail(2)
	if err != nil {
		t.Fatalf("tail limit: %v", err)
	}
	if len(top2) != 2 || top2[0].DecisionKind != "k4" || top2[1].DecisionKind != "k3" {
		t.Fatalf("limit=2 must take the newest 2; got %+v", top2)
	}
}

func TestAuditSinkTailEmptyTableNoError(t *testing.T) {
	l := openLedger(t, nil)
	sink := l.AuditSink()
	recs, err := sink.Tail(10)
	if err != nil {
		t.Fatalf("tail on empty table must NOT error; got %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("empty table => empty tail, got %d", len(recs))
	}
}

func TestAuditSinkTailPerformsNoMutation(t *testing.T) {
	l := openLedger(t, nil)
	sink := l.AuditSink()
	for i := 0; i < 3; i++ {
		if err := sink.Append(daemon.AuditRecord{
			Timestamp: time.Now(), Session: "s", DecisionProject: "p", DecisionKind: "k",
		}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := l.AuditCount()
	if err != nil {
		t.Fatal(err)
	}
	// Several tails, including a non-positive limit (clamped, never a panic).
	for _, lim := range []int{0, -1, 1, 2, 100} {
		if _, err := sink.Tail(lim); err != nil {
			t.Fatalf("tail(%d): %v", lim, err)
		}
	}
	after, err := l.AuditCount()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("Tail mutated the audit_log: count %d -> %d", before, after)
	}
}
