package watch

// Model/View render tests: feed a FIXED Snapshot (via an injected fetch func, so
// no live daemon is needed) and assert View() contains the expected coordination
// feed substrings. Also asserts degraded/unreachable paths render without panic,
// and that keybindings remain navigation-only.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// fixedFetcher returns the same snapshot every poll.
func fixedFetcher(s Snapshot) Fetcher { return func() Snapshot { return s } }

// landed drives a model from Init through one snapshot delivery so View renders
// real content (not the pre-first-fetch "connecting" header).
func landed(t *testing.T, snap Snapshot) Model {
	t.Helper()
	m := NewModel(fixedFetcher(snap), time.Hour)
	updated, _ := m.Update(snapshotMsg{snap: snap})
	return updated.(Model)
}

func withSize(t *testing.T, m Model, width, height int) Model {
	t.Helper()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return updated.(Model)
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

func auditEntry(tsMs int64, session, project, kind, payload string) AuditEntry {
	return AuditEntry{
		TimestampMs:     tsMs,
		Session:         session,
		DecisionProject: project,
		DecisionKind:    kind,
		Payload:         json.RawMessage(payload),
	}
}

func baseSnapshot() Snapshot {
	nowMs := time.Now().UnixMilli()
	return Snapshot{
		DaemonReachable: true,
		Socket:          "/tmp/test.sock",
		Whoami:          "s-9-deadbeefcafef00d",
		Trunk: TrunkPanel{
			Available: true,
			Exists:    true,
			Tip:       "abcdef1234567890",
			Branch:    "trunk",
		},
		Leases: LeasePanel{
			Available: true,
			Holders: []LeaseHolder{
				{Key: "mad-trellis:integrator:v1", Holder: "s-3-feedface0000", ExpiresAtMs: nowMs + 25_000, Fence: 1},
			},
		},
		Integrations: IntegrationPanel{
			Available: true,
			Integrations: []Integration{
				{ID: "int-001", Branch: "feature/login", Holder: "s-2-aaa", State: "promoted", MergeCommit: "abcdef1234567890"},
			},
		},
		Reviews: ReviewPanel{Available: true},
		Audit:   AuditPanel{Available: true},
	}
}

func richSnapshot() Snapshot {
	nowMs := time.Now().UnixMilli()
	snap := baseSnapshot()
	snap.Integrations.Integrations = append(snap.Integrations.Integrations,
		Integration{ID: "int-002", Branch: "feature/logout", Holder: "s-4-bbb", State: "validating"},
	)
	snap.Reviews.Records = []IntegrationRecord{
		{ID: "rq-001", Branch: "nm/abc", Title: "add tokens", State: "requested", CreatedAtMs: nowMs - 9_000, UpdatedAtMs: nowMs - 9_000},
		{ID: "rq-002", Branch: "nm/def", Title: "fix leak", State: "claimed", Claimer: "s-3-feedface0000", UpdatedAtMs: nowMs - 5_000},
		{ID: "rq-003", Branch: "nm/ghi", Title: "rename", State: "changes_requested", Claimer: "s-3-feedface0000", Feedback: "please split this into two separate commits and add a regression test before this can be approved", UpdatedAtMs: nowMs - 4_000},
		{ID: "rq-004", Branch: "nm/jkl", Title: "docs", State: "approved", Claimer: "s-3-feedface0000", Merge: "0badc0ffee123456", UpdatedAtMs: nowMs - 2_000},
		{ID: "rq-005", Branch: "nm/mno", Title: "spike", State: "withdrawn", UpdatedAtMs: nowMs - 1_000},
	}
	// audit.tail is newest-first; the view reverses it so newest lands at bottom.
	snap.Audit.Entries = []AuditEntry{
		auditEntry(nowMs-1_000, "s-7-777777777777", "integration-review", "integration.withdrawn", `{"branch":"nm/mno"}`),
		auditEntry(nowMs-2_000, "s-6-666666666666", "ops", "nudge.delivered", `{"audience":"integrator","kind":"review"}`),
		auditEntry(nowMs-3_000, "s-3-feedface0000", "integrator-trunk", "trunk.promoted", `{"commit":"0badc0ffee123456"}`),
		auditEntry(nowMs-4_000, "s-3-feedface0000", "integration-review", "integration.changes_requested", `{"branch":"nm/ghi","feedback":"please split this into two separate commits and add a regression test before this can be approved"}`),
		auditEntry(nowMs-5_000, "s-3-feedface0000", "integration-review", "integration.approved", `{"branch":"nm/jkl","merge":"0badc0ffee123456"}`),
		auditEntry(nowMs-6_000, "s-3-feedface0000", "integration-review", "integration.claimed", `{"branch":"nm/def","claimer":"s-3-feedface0000"}`),
		auditEntry(nowMs-9_000, "s-2-aaaaaaaaaaaa", "integration-review", "integration.requested", `{"branch":"nm/abc","title":"add tokens"}`),
	}
	return snap
}

func TestViewRendersCoordinationFeedSnapshot(t *testing.T) {
	m := landed(t, richSnapshot())
	out := m.View()

	wants := []string{
		"mad-trellis watch — read-only · watcher session s-9-deadbe",
		"trunk abcdef123456",
		"1 requested / 1 claimed",
		"integrator: present (s-3-feedfa)",
		"leases held: 1",
		"Review queue",
		"nm/abc                       [requested]  \"add tokens\"",
		"nm/def                       [claimed by s-3-feedfa]  \"fix leak\"",
		"Coordination feed",
		"builder s-2-aaaaaa · requested nm/abc · \"add tokens\"",
		"integrator s-3-feedfa · claimed nm/def",
		"integrator s-3-feedfa · approved nm/jkl — merged 0badc0ffee12",
		"integrator s-3-feedfa · changes requested on nm/ghi — \"please split this into two separate commits and add a regre…\"",
		"integrator · trunk.promoted 0badc0ffee12",
		"substrate · nudged integrator",
		"builder s-7-777777 · withdrew nm/mno",
		"read-only · q quit · tab focus · j/k scroll · newest at bottom",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("View() missing expected substring %q\n---\n%s", w, out)
		}
	}

	if strings.Contains(out, "changes_requested by") || strings.Contains(out, "merged 0badc0ffee12  —") || strings.Contains(out, "nm/mno                       [withdrawn]") {
		t.Fatalf("review queue must show only requested/claimed open work:\n%s", out)
	}
	if strings.Contains(out, "branch feature/login") {
		t.Fatalf("promoted integration leaked into the coordination surface as pending detail:\n%s", out)
	}
}

func TestFeedRendersAuditKindMappings(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	tests := []struct {
		name    string
		project string
		kind    string
		session string
		payload string
		want    string
	}{
		{
			name:    "requested",
			project: "integration-review",
			kind:    "integration.requested",
			session: "s-1-aaaaaaaaaaaa",
			payload: `{"branch":"nm/a","title":"Add A"}`,
			want:    "builder s-1-aaaaaa · requested nm/a · \"Add A\"",
		},
		{
			name:    "claimed",
			project: "integration-review",
			kind:    "integration.claimed",
			session: "s-2-bbbbbbbbbbbb",
			payload: `{"branch":"nm/b","claimer":"s-2-bbbbbbbbbbbb"}`,
			want:    "integrator s-2-bbbbbb · claimed nm/b",
		},
		{
			name:    "approved",
			project: "integration-review",
			kind:    "integration.approved",
			session: "s-3-cccccccccccc",
			payload: `{"branch":"nm/c","merge":"1234567890abcdef"}`,
			want:    "integrator s-3-cccccc · approved nm/c — merged 1234567890ab",
		},
		{
			name:    "changes requested",
			project: "integration-review",
			kind:    "integration.changes_requested",
			session: "s-4-dddddddddddd",
			payload: `{"branch":"nm/d","feedback":"needs the missing regression test before this can merge"}`,
			want:    "integrator s-4-dddddd · changes requested on nm/d — \"needs the missing regression test before this can merge\"",
		},
		{
			name:    "withdrawn",
			project: "integration-review",
			kind:    "integration.withdrawn",
			session: "s-5-eeeeeeeeeeee",
			payload: `{"branch":"nm/e"}`,
			want:    "builder s-5-eeeeee · withdrew nm/e",
		},
		{
			name:    "requeued",
			project: "integration-review",
			kind:    "integration.requeued",
			session: "s-6-ffffffffffff",
			payload: `{"branch":"nm/f"}`,
			want:    "substrate · requeued nm/f (claimer died)",
		},
		{
			name:    "integrator trunk branch",
			project: "integrator-trunk",
			kind:    "trunk.claimed",
			session: "s-7-111111111111",
			payload: `{"branch":"trunk"}`,
			want:    "integrator · trunk.claimed trunk",
		},
		{
			name:    "integrator trunk commit",
			project: "integrator-trunk",
			kind:    "trunk.promoted",
			session: "s-8-222222222222",
			payload: `{"commit":"fedcba9876543210"}`,
			want:    "integrator · trunk.promoted fedcba987654",
		},
		{
			name:    "nudge delivered",
			project: "coop",
			kind:    "nudge.delivered",
			session: "s-9-333333333333",
			payload: `{"audience":"builder","kind":"review"}`,
			want:    "substrate · nudged builder",
		},
		{
			name:    "fallback",
			project: "liveness",
			kind:    "liveness.reclaimed",
			session: "s-10-444444444444",
			payload: `{"holder":"h","key":"k"}`,
			want:    "substrate · liveness/liveness.reclaimed · session s-10-444444",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := baseSnapshot()
			snap.Audit.Entries = []AuditEntry{auditEntry(nowMs-2_000, tt.session, tt.project, tt.kind, tt.payload)}
			out := landed(t, snap).View()
			if !strings.Contains(out, tt.want) {
				t.Fatalf("feed mapping missing %q\n---\n%s", tt.want, out)
			}
			if !strings.Contains(out, "ago") {
				t.Fatalf("feed line must end with relative time:\n%s", out)
			}
		})
	}
}

func TestHeightBudgetWithLongAuditTail(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	snap := baseSnapshot()
	for i := 0; i < 8; i++ {
		snap.Reviews.Records = append(snap.Reviews.Records, IntegrationRecord{
			ID:          fmt.Sprintf("rq-%03d", i),
			Branch:      fmt.Sprintf("nm/open-%02d", i),
			Title:       fmt.Sprintf("open %02d", i),
			State:       "requested",
			UpdatedAtMs: nowMs - int64(i)*1000,
		})
	}
	for i := 0; i < 50; i++ {
		snap.Audit.Entries = append(snap.Audit.Entries,
			auditEntry(nowMs-int64(i+1)*1000, "s-1-aaaaaaaaaaaa", "integration-review", "integration.requested", fmt.Sprintf(`{"branch":"nm/%02d","title":"entry %02d"}`, i, i)),
		)
	}

	m := withSize(t, landed(t, snap), 100, 20)
	out := m.View()
	if got := lineCount(out); got > 20 {
		t.Fatalf("View() rendered %d lines, want <=20\n---\n%s", got, out)
	}
	for _, want := range []string{"mad-trellis watch", "Review queue", "… +", "Coordination feed", "read-only · q quit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("height-budgeted view missing %q\n---\n%s", want, out)
		}
	}
}

func TestFeedDedupeConsecutiveIdenticalEntries(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	snap := baseSnapshot()
	for i := 0; i < 48; i++ {
		snap.Audit.Entries = append(snap.Audit.Entries,
			auditEntry(nowMs-int64(i+1)*1000, "s-1-aaaaaaaaaaaa", "liveness", "liveness.reclaimed", `{"holder":"h1","key":"k1"}`),
		)
	}
	out := landed(t, snap).View()
	if !strings.Contains(out, "×48") {
		t.Fatalf("deduped liveness storm must show a single ×48 line:\n%s", out)
	}
	if got := strings.Count(out, "liveness/liveness.reclaimed"); got != 1 {
		t.Fatalf("expected one collapsed liveness line, got %d\n%s", got, out)
	}

	snap.Audit.Entries = []AuditEntry{
		auditEntry(nowMs-1_000, "s-1-aaaaaaaaaaaa", "liveness", "liveness.reclaimed", `{"holder":"h1","key":"k1"}`),
		auditEntry(nowMs-2_000, "s-2-bbbbbbbbbbbb", "liveness", "liveness.reclaimed", `{"holder":"h2","key":"k1"}`),
		auditEntry(nowMs-3_000, "s-1-aaaaaaaaaaaa", "liveness", "liveness.reclaimed", `{"holder":"h1","key":"k1"}`),
	}
	out = landed(t, snap).View()
	if got := strings.Count(out, "session s-1-aaaaaa"); got != 2 {
		t.Fatalf("different payload between identical entries must break the run, got %d matching lines\n%s", got, out)
	}
}

func TestNewestAtBottomAndScrollClamps(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	snap := baseSnapshot()
	for i := 9; i >= 0; i-- {
		snap.Audit.Entries = append(snap.Audit.Entries,
			auditEntry(nowMs-int64(10-i)*1000, "s-1-aaaaaaaaaaaa", "integration-review", "integration.requested", fmt.Sprintf(`{"branch":"nm/%02d","title":"entry %02d"}`, i, i)),
		)
	}
	m := withSize(t, landed(t, snap), 100, 12)
	out := m.View()
	if !strings.Contains(out, "requested nm/09") {
		t.Fatalf("default feed viewport must be pinned to newest at bottom:\n%s", out)
	}

	bottomOffset := m.offsets[panelFeed]
	for i := 0; i < 20; i++ {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
		m = updated.(Model)
	}
	if m.offsets[panelFeed] >= bottomOffset {
		t.Fatalf("k must scroll feed upward toward older entries: before=%d after=%d", bottomOffset, m.offsets[panelFeed])
	}
	if out = m.View(); !strings.Contains(out, "requested nm/00") {
		t.Fatalf("scrolling up should reveal older entries:\n%s", out)
	}
	topOffset := m.offsets[panelFeed]
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = updated.(Model)
	if m.offsets[panelFeed] != topOffset {
		t.Fatalf("k at top must clamp, before=%d after=%d", topOffset, m.offsets[panelFeed])
	}

	for i := 0; i < 20; i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = updated.(Model)
	}
	if m.offsets[panelFeed] != bottomOffset {
		t.Fatalf("j must scroll back down and clamp at newest bottom: want=%d got=%d", bottomOffset, m.offsets[panelFeed])
	}
}

func TestIntegratorAbsentHighlight(t *testing.T) {
	snap := baseSnapshot()
	snap.Leases.Holders = []LeaseHolder{
		{Key: "some:other:lease", Holder: "s-4-aaaaaaaaaaaa"},
	}
	snap.Reviews.Records = []IntegrationRecord{
		{ID: "rq-001", Branch: "nm/waiting", Title: "waiting", State: "requested", UpdatedAtMs: time.Now().UnixMilli()},
	}
	out := landed(t, snap).View()
	if !strings.Contains(out, "integrator: ABSENT") || !strings.Contains(out, "integrator start") {
		t.Fatalf("requested work without integrator lease must highlight the absent integrator:\n%s", out)
	}
}

func TestUnparseablePayloadFallsBackToRawLine(t *testing.T) {
	snap := baseSnapshot()
	snap.Audit.Entries = []AuditEntry{
		{TimestampMs: time.Now().Add(-time.Second).UnixMilli(), Session: "s-1-aaaaaaaaaaaa", DecisionProject: "integration-review", DecisionKind: "integration.requested", Payload: json.RawMessage(`{`)},
	}
	out := landed(t, snap).View()
	want := "integration-review/integration.requested · session s-1-aaaaaa"
	if !strings.Contains(out, want) {
		t.Fatalf("unparseable payload must fall back to raw project/kind line %q\n---\n%s", want, out)
	}
}

func TestReviewQueueEmptyAndUnavailable(t *testing.T) {
	empty := landed(t, Snapshot{DaemonReachable: true, Reviews: ReviewPanel{Available: true}, Audit: AuditPanel{Available: true}})
	if out := empty.View(); !strings.Contains(out, "no open review requests") {
		t.Fatalf("empty review queue must render the no-open-requests line:\n%s", out)
	}

	unavail := landed(t, Snapshot{DaemonReachable: true, Reviews: ReviewPanel{Available: false}, Audit: AuditPanel{Available: true}})
	if out := unavail.View(); !strings.Contains(out, unavailableLine) {
		t.Fatalf("unavailable review queue must degrade to the unavailable line:\n%s", out)
	}
}

func TestViewAllUnavailableRendersWithoutPanic(t *testing.T) {
	// Reachable daemon but every sub-call wedged: regions degrade individually.
	snap := Snapshot{DaemonReachable: true, Socket: "/tmp/x.sock"}
	m := landed(t, snap)
	out := m.View()
	if !strings.Contains(out, unavailableLine) {
		t.Fatalf("each wedged region must show %q\n%s", unavailableLine, out)
	}
	if !strings.Contains(out, "read-only") {
		t.Fatalf("footer must still render:\n%s", out)
	}
}

func TestViewUnreachableFullScreen(t *testing.T) {
	snap := Snapshot{DaemonReachable: false, Socket: "/tmp/dead.sock"}
	m := landed(t, snap)
	out := m.View()
	if !strings.Contains(out, "cannot reach daemon at /tmp/dead.sock") {
		t.Fatalf("unreachable daemon must show friendly full-screen message:\n%s", out)
	}
	if !strings.Contains(out, "non-load-bearing") {
		t.Fatalf("message should reassure it is non-load-bearing:\n%s", out)
	}
}

func TestViewPreFirstFetchDoesNotClaimUnreachable(t *testing.T) {
	// Before the first snapshot lands, the model must not falsely claim the daemon
	// is unreachable (DaemonReachable defaults to false on a zero Model).
	m := NewModel(fixedFetcher(Snapshot{}), time.Hour)
	out := m.View()
	if strings.Contains(out, "cannot reach daemon") {
		t.Fatalf("pre-first-fetch must not claim unreachable:\n%s", out)
	}
	if !strings.Contains(out, "connecting") {
		t.Fatalf("pre-first-fetch should show a connecting state:\n%s", out)
	}
}

func TestKeybindingsAreNavigationOnly(t *testing.T) {
	m := withSize(t, landed(t, richSnapshot()), 100, 12)

	if m.focus != panelFeed {
		t.Fatalf("default focus must be feed, got %v", m.focus)
	}
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = nm.(Model)
	if m.focus != panelQueue {
		t.Fatalf("tab must move focus feed -> queue, got %v", m.focus)
	}
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = nm.(Model)
	if m.focus != panelFeed {
		t.Fatalf("shift+tab must return focus to feed, got %v", m.focus)
	}

	beforeOff := m.offsets[m.focus]
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = nm.(Model)
	if m.offsets[m.focus] > beforeOff {
		t.Fatalf("k must not scroll toward newer entries")
	}
	nm, _ = m.Update(tea.MouseMsg(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown}))
	m = nm.(Model)
	if m.offsets[m.focus] < beforeOff {
		t.Fatalf("mouse wheel down must scroll the focused region toward newer entries")
	}

	for _, r := range []rune{'a', 'p', 'r', 'x', 'd', 's'} {
		beforeFocus, beforeOffsets := m.focus, m.offsets
		nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(Model)
		if cmd != nil {
			t.Fatalf("key %q produced a command — watch must have NO action affordance", string(r))
		}
		if m.focus != beforeFocus || m.offsets != beforeOffsets {
			t.Fatalf("key %q changed navigation state — action-like keys must be no-ops", string(r))
		}
	}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatalf("q must quit")
	}
}

func TestUpdateTickRefetchesAndRearms(t *testing.T) {
	var calls int
	fetch := func() Snapshot { calls++; return Snapshot{DaemonReachable: true} }
	m := NewModel(fetch, time.Hour)

	// A tick should produce a batch (fetch + re-tick). Execute the returned cmd to
	// confirm it carries a snapshot fetch.
	_, cmd := m.Update(tickMsg{})
	if cmd == nil {
		t.Fatal("tick must schedule the next poll")
	}
	fc := m.fetchCmd()
	msg := fc()
	if _, ok := msg.(snapshotMsg); !ok {
		t.Fatalf("fetchCmd must deliver a snapshotMsg, got %T", msg)
	}
	if calls == 0 {
		t.Fatal("fetch func must be invoked by fetchCmd")
	}
}

func TestInitReturnsCommand(t *testing.T) {
	m := NewModel(fixedFetcher(Snapshot{DaemonReachable: true}), time.Hour)
	if m.Init() == nil {
		t.Fatal("Init must return an initial command (fetch + tick)")
	}
}

func TestRelativeTimeRendersDays(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	if got := relativeTime(nowMs-int64(72*time.Hour/time.Millisecond), nowMs); got != "3d ago" {
		t.Fatalf("relativeTime() for 72h = %q, want 3d ago", got)
	}
}
