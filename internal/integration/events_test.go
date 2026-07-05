package integration

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestIntegrationWithOptions(t *testing.T, opts Options) *Integration {
	t.Helper()
	if opts.StorePath == "" {
		opts.StorePath = filepath.Join(t.TempDir(), "integration.db")
	}
	ig, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = ig.Close() })
	return ig
}

func TestTransitionsAppendEvents(t *testing.T) {
	ig := newTestIntegrationWithOptions(t, Options{})

	if _, err := ig.Request("builder", "nm/builder", "Build it"); err != nil {
		t.Fatalf("request: %v", err)
	}
	assertEventRows(t, ig, []eventWant{
		{Audience: "integrator", Kind: "integration.requested", Branch: "nm/builder"},
	})

	ok, _, err := ig.Claim("integrator", "nm/builder")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	assertEventRows(t, ig, []eventWant{
		{Audience: "integrator", Kind: "integration.requested", Branch: "nm/builder"},
		{Audience: "branch:nm/builder", Kind: "integration.claimed", Branch: "nm/builder"},
	})

	ok, state, err := ig.Verdict("integrator", "nm/builder", "approve", "", "merge-1")
	if err != nil || !ok || state != StateApproved {
		t.Fatalf("approve: ok=%v state=%q err=%v", ok, state, err)
	}
	assertEventRows(t, ig, []eventWant{
		{Audience: "integrator", Kind: "integration.requested", Branch: "nm/builder"},
		{Audience: "branch:nm/builder", Kind: "integration.claimed", Branch: "nm/builder"},
		{Audience: "branch:nm/builder", Kind: "integration.verdict", Branch: "nm/builder"},
	})

	if _, err := ig.Request("builder-2", "nm/builder-2", ""); err != nil {
		t.Fatalf("request 2: %v", err)
	}
	ok, _, err = ig.Claim("dead-integrator", "nm/builder-2")
	if err != nil || !ok {
		t.Fatalf("claim 2: ok=%v err=%v", ok, err)
	}
	n, err := ig.ReclaimStaleClaims(func(session string) bool { return session == "dead-integrator" })
	if err != nil || n != 1 {
		t.Fatalf("reclaim stale claims: n=%d err=%v", n, err)
	}
	assertEventRows(t, ig, []eventWant{
		{Audience: "integrator", Kind: "integration.requested", Branch: "nm/builder"},
		{Audience: "branch:nm/builder", Kind: "integration.claimed", Branch: "nm/builder"},
		{Audience: "branch:nm/builder", Kind: "integration.verdict", Branch: "nm/builder"},
		{Audience: "integrator", Kind: "integration.requested", Branch: "nm/builder-2"},
		{Audience: "branch:nm/builder-2", Kind: "integration.claimed", Branch: "nm/builder-2"},
		{Audience: "integrator", Kind: "integration.requeued", Branch: "nm/builder-2"},
	})
}

func TestEventsCursorReadAndAdvance(t *testing.T) {
	ig := newTestIntegrationWithOptions(t, Options{
		HoldsIntegratorPresence: func(session string) bool { return session == "integrator" },
	})
	for _, branch := range []string{"br-1", "br-2"} {
		if _, err := ig.Request("builder", branch, ""); err != nil {
			t.Fatalf("request %s: %v", branch, err)
		}
	}

	first := callEvents(t, ig, "integrator", map[string]any{"max": 50})
	if gotKinds(first); strings.Join(gotKinds(first), ",") != "integration.requested,integration.requested" {
		t.Fatalf("first poll must return both requested events; got %+v", first)
	}
	if first[0].ID == 0 || first[1].ID <= first[0].ID {
		t.Fatalf("events must carry increasing ids; got %+v", first)
	}

	second := callEvents(t, ig, "integrator", map[string]any{"max": 50})
	if len(second) != 0 {
		t.Fatalf("second poll must advance cursor and return no events; got %+v", second)
	}

	if _, err := ig.Request("builder", "br-3", ""); err != nil {
		t.Fatalf("request br-3: %v", err)
	}
	third := callEvents(t, ig, "integrator", map[string]any{"max": 50})
	if len(third) != 1 || third[0].Branch != "br-3" || third[0].Kind != "integration.requested" {
		t.Fatalf("third poll must return only the new event; got %+v", third)
	}
}

func TestEventsAuthorizationNonVacuous(t *testing.T) {
	ig := newTestIntegrationWithOptions(t, Options{
		HoldsIntegratorPresence: func(session string) bool { return session == "present-integrator" },
	})

	if _, err := ig.Request("holder-session", "nm/launcher-session", ""); err != nil {
		t.Fatalf("request launcher branch: %v", err)
	}
	if ok, _, err := ig.Claim("reviewer", "nm/launcher-session"); err != nil || !ok {
		t.Fatalf("claim launcher branch: ok=%v err=%v", ok, err)
	}
	if got := callEvents(t, ig, "stranger", map[string]any{"branch": "nm/launcher-session"}); len(got) != 0 {
		t.Fatalf("unauthorized branch consumer must receive no events; got %+v", got)
	}
	got := callEvents(t, ig, "launcher-session", map[string]any{"branch": "nm/launcher-session"})
	if len(got) != 1 || got[0].Kind != "integration.claimed" {
		t.Fatalf("launcher-pattern session must receive the branch event; got %+v", got)
	}

	if _, err := ig.Request("holder-session", "feature/held", ""); err != nil {
		t.Fatalf("request held branch: %v", err)
	}
	if ok, _, err := ig.Claim("reviewer", "feature/held"); err != nil || !ok {
		t.Fatalf("claim held branch: ok=%v err=%v", ok, err)
	}
	got = callEvents(t, ig, "holder-session", map[string]any{"branch": "feature/held"})
	if len(got) != 1 || got[0].Kind != "integration.claimed" {
		t.Fatalf("record holder must receive the branch event; got %+v", got)
	}

	if _, err := ig.Request("holder-session", "feature/presence", ""); err != nil {
		t.Fatalf("request presence branch: %v", err)
	}
	if got := callEvents(t, ig, "no-presence", map[string]any{"branch": "feature/presence"}); len(got) != 0 {
		t.Fatalf("consumer without integrator presence must receive no integrator events; got %+v", got)
	}
	got = callEvents(t, ig, "present-integrator", map[string]any{"branch": "feature/presence"})
	if len(got) != 1 || got[0].Kind != "integration.requested" {
		t.Fatalf("consumer with integrator presence must receive the integrator event; got %+v", got)
	}
}

func TestEventAppendFailureDoesNotFailTransition(t *testing.T) {
	ig := newTestIntegrationWithOptions(t, Options{})
	if _, err := ig.store.db.Exec(`DROP TABLE events`); err != nil {
		t.Fatalf("drop events table: %v", err)
	}
	rec, err := ig.Request("builder", "feature/no-events-table", "still records truth")
	if err != nil {
		t.Fatalf("request must not fail when event append fails: %v", err)
	}
	if rec.State != StateRequested {
		t.Fatalf("request must still land in durable truth rows; got %+v", rec)
	}
	got, found, err := ig.Status("feature/no-events-table")
	if err != nil || !found || got.Holder != "builder" {
		t.Fatalf("status after event append failure: found=%v rec=%+v err=%v", found, got, err)
	}
}

func TestGCDeletesOnlyEventsOlderThan24HoursAndKeepsCursors(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &fakeClock{t: base}
	ig := newTestIntegrationWithOptions(t, Options{
		Clock:                   clk,
		HoldsIntegratorPresence: func(string) bool { return true },
	})

	if _, err := ig.Request("builder", "old-event", ""); err != nil {
		t.Fatalf("old request: %v", err)
	}
	if got := callEvents(t, ig, "consumer", nil); len(got) != 1 {
		t.Fatalf("setup poll must create a cursor over the old event; got %+v", got)
	}
	clk.advance(23 * time.Hour)
	if _, err := ig.Request("builder", "young-event", ""); err != nil {
		t.Fatalf("young request: %v", err)
	}
	clk.advance(2 * time.Hour)

	if _, err := ig.GCStale(); err != nil {
		t.Fatalf("GCStale: %v", err)
	}
	rows := eventRows(t, ig)
	if len(rows) != 1 || rows[0].Branch != "young-event" {
		t.Fatalf("GC must delete only >24h-old events and keep young ones; got %+v", rows)
	}
	var cursors int
	if err := ig.store.db.QueryRow(`SELECT COUNT(*) FROM event_cursors`).Scan(&cursors); err != nil {
		t.Fatalf("count cursors: %v", err)
	}
	if cursors != 1 {
		t.Fatalf("event GC must leave cursor rows alone; got %d", cursors)
	}
}

func TestIntegrationAuditPayloads(t *testing.T) {
	var got []auditCall
	ig := newTestIntegrationWithOptions(t, Options{
		Audit: func(session, kind string, payload []byte) {
			got = append(got, auditCall{Session: session, Kind: kind, Payload: append([]byte(nil), payload...)})
		},
	})

	longTitle := strings.Repeat("t", 125)
	longFeedback := strings.Repeat("f", 205)

	if _, err := ig.Request("builder", "br-approved", longTitle); err != nil {
		t.Fatalf("request approved branch: %v", err)
	}
	if ok, _, err := ig.Claim("claimer", "br-approved"); err != nil || !ok {
		t.Fatalf("claim approved branch: ok=%v err=%v", ok, err)
	}
	if ok, _, err := ig.Verdict("claimer", "br-approved", "approve", "", "merge-oid"); err != nil || !ok {
		t.Fatalf("approve branch: ok=%v err=%v", ok, err)
	}

	if _, err := ig.Request("builder", "br-rejected", ""); err != nil {
		t.Fatalf("request rejected branch: %v", err)
	}
	if ok, _, err := ig.Claim("claimer", "br-rejected"); err != nil || !ok {
		t.Fatalf("claim rejected branch: ok=%v err=%v", ok, err)
	}
	if ok, _, err := ig.Verdict("claimer", "br-rejected", "reject", longFeedback, ""); err != nil || !ok {
		t.Fatalf("reject branch: ok=%v err=%v", ok, err)
	}

	if _, err := ig.Request("builder", "br-withdrawn", ""); err != nil {
		t.Fatalf("request withdrawn branch: %v", err)
	}
	if ok, err := ig.Cancel("builder", "br-withdrawn"); err != nil || !ok {
		t.Fatalf("cancel branch: ok=%v err=%v", ok, err)
	}

	if _, err := ig.Request("builder", "br-requeued", ""); err != nil {
		t.Fatalf("request requeued branch: %v", err)
	}
	if ok, _, err := ig.Claim("dead-reviewer", "br-requeued"); err != nil || !ok {
		t.Fatalf("claim requeued branch: ok=%v err=%v", ok, err)
	}
	if n, err := ig.ReclaimStaleClaims(func(session string) bool { return session == "dead-reviewer" }); err != nil || n != 1 {
		t.Fatalf("requeue branch: n=%d err=%v", n, err)
	}

	requested := auditPayloadFor(t, got, "integration.requested", "br-approved")
	if requested["title"] != strings.Repeat("t", 120) {
		t.Fatalf("requested audit title must be truncated to 120 runes; got len=%d", len([]rune(requested["title"])))
	}
	claimed := auditPayloadFor(t, got, "integration.claimed", "br-approved")
	if claimed["claimer"] != "claimer" {
		t.Fatalf("claimed audit payload must include claimer; got %v", claimed)
	}
	approved := auditPayloadFor(t, got, "integration.approved", "br-approved")
	if approved["merge"] != "merge-oid" {
		t.Fatalf("approved audit payload must include merge; got %v", approved)
	}
	rejected := auditPayloadFor(t, got, "integration.changes_requested", "br-rejected")
	if rejected["feedback"] != strings.Repeat("f", 200) {
		t.Fatalf("changes_requested feedback must be truncated to 200 runes; got len=%d", len([]rune(rejected["feedback"])))
	}
	withdrawn := auditPayloadFor(t, got, "integration.withdrawn", "br-withdrawn")
	if withdrawn["branch"] != "br-withdrawn" {
		t.Fatalf("withdrawn audit payload must include branch; got %v", withdrawn)
	}
	requeued := auditPayloadFor(t, got, "integration.requeued", "br-requeued")
	if requeued["branch"] != "br-requeued" {
		t.Fatalf("requeued audit payload must include branch; got %v", requeued)
	}
}

func TestPendingHandlerReplyFieldsPinned(t *testing.T) {
	ig := newTestIntegrationWithOptions(t, Options{})
	if _, perr := requestHandler(ig)(ccFor("builder"), mustParams(t, map[string]any{
		"branch": "feature/pending-fields",
		"title":  "Pinned",
	})); perr != nil {
		t.Fatalf("request: %+v", perr)
	}
	raw, perr := pendingHandler(ig)(ccFor("integrator"), nil)
	if perr != nil {
		t.Fatalf("pending: %+v", perr)
	}
	var out struct {
		Pending []map[string]json.RawMessage `json:"pending"`
	}
	mustJSON(t, raw, &out)
	if len(out.Pending) != 1 {
		t.Fatalf("expected one pending entry; got %+v", out.Pending)
	}
	want := map[string]bool{"id": true, "branch": true, "title": true, "state": true, "created_at_ms": true}
	if len(out.Pending[0]) != len(want) {
		t.Fatalf("pending fields changed: got keys %v", keys(out.Pending[0]))
	}
	for k := range out.Pending[0] {
		if !want[k] {
			t.Fatalf("pending fields changed: unexpected key %q in %v", k, keys(out.Pending[0]))
		}
	}
}

type eventWant struct {
	Audience string
	Kind     string
	Branch   string
}

func assertEventRows(t *testing.T, ig *Integration, wants []eventWant) {
	t.Helper()
	rows := eventRows(t, ig)
	if len(rows) != len(wants) {
		t.Fatalf("event count: got %d rows %+v, want %d", len(rows), rows, len(wants))
	}
	for i, want := range wants {
		if rows[i].Audience != want.Audience || rows[i].Kind != want.Kind || rows[i].Branch != want.Branch {
			t.Fatalf("event %d: got audience=%q kind=%q branch=%q, want %+v", i, rows[i].Audience, rows[i].Kind, rows[i].Branch, want)
		}
		if rows[i].ID == 0 || rows[i].CreatedAt.IsZero() {
			t.Fatalf("event %d must have id and created_at; got %+v", i, rows[i])
		}
	}
}

func eventRows(t *testing.T, ig *Integration) []Event {
	t.Helper()
	rows, err := ig.store.db.Query(`SELECT id, audience, kind, branch, created_at_ns FROM events ORDER BY id`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var created int64
		if err := rows.Scan(&ev.ID, &ev.Audience, &ev.Kind, &ev.Branch, &created); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		ev.CreatedAt = time.Unix(0, created)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	return out
}

type eventReply struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	Branch      string `json:"branch"`
	CreatedAtMS int64  `json:"created_at_ms"`
}

func callEvents(t *testing.T, ig *Integration, session string, params any) []eventReply {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		raw = mustParams(t, params)
	}
	got, perr := eventsHandler(ig)(ccFor(session), raw)
	if perr != nil {
		t.Fatalf("integration.events: %+v", perr)
	}
	var out struct {
		Events []eventReply `json:"events"`
	}
	mustJSON(t, got, &out)
	return out.Events
}

func gotKinds(events []eventReply) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Kind)
	}
	return out
}

type auditCall struct {
	Session string
	Kind    string
	Payload []byte
}

func auditPayloadFor(t *testing.T, calls []auditCall, kind, branch string) map[string]string {
	t.Helper()
	for _, c := range calls {
		if c.Kind != kind {
			continue
		}
		var payload map[string]string
		if err := json.Unmarshal(c.Payload, &payload); err != nil {
			t.Fatalf("unmarshal audit payload for %s: %v", kind, err)
		}
		if payload["branch"] == branch {
			return payload
		}
	}
	t.Fatalf("missing audit call kind=%s branch=%s in %+v", kind, branch, calls)
	return nil
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
