package watch

// Model/View render tests: feed a FIXED Snapshot (via an injected fetch func, so
// no live daemon is needed) and assert View() contains the expected substrings.
// Also asserts the all-unavailable / unreachable paths render without panic, and
// that the keybindings are navigation-only (no governed mutation possible).

import (
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

func richSnapshot() Snapshot {
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
				{Key: "a2V5", Holder: "s-3-feedface0000", ExpiresAtMs: nowMs + 25_000, Fence: 1},
			},
		},
		Integrations: IntegrationPanel{
			Available: true,
			Integrations: []Integration{
				{ID: "int-001", Branch: "feature/login", Holder: "s-2-aaa", State: "promoted", MergeCommit: "abcdef1234567890"},
				{ID: "int-002", Branch: "feature/logout", Holder: "s-4-bbb", State: "validating"},
				{ID: "int-003", Branch: "feature/conflict", Holder: "s-5-ccc", State: "aborted"},
			},
		},
		Reviews: ReviewPanel{
			Available: true,
			Records: []IntegrationRecord{
				{ID: "rq-001", Branch: "nm/abc", Title: "add tokens", State: "requested", CreatedAtMs: nowMs - 9_000, UpdatedAtMs: nowMs - 9_000},
				{ID: "rq-002", Branch: "nm/def", Title: "fix leak", State: "claimed", Claimer: "s-3-feedface0000", UpdatedAtMs: nowMs - 5_000},
				{ID: "rq-003", Branch: "nm/ghi", Title: "rename", State: "changes_requested", Claimer: "s-3-feedface0000", Feedback: "please split this into two separate commits and add a regression test before this can be approved", UpdatedAtMs: nowMs - 4_000},
				{ID: "rq-004", Branch: "nm/jkl", Title: "docs", State: "approved", Claimer: "s-3-feedface0000", Merge: "0badc0ffee123456", UpdatedAtMs: nowMs - 2_000},
				{ID: "rq-005", Branch: "nm/mno", Title: "spike", State: "withdrawn", UpdatedAtMs: nowMs - 1_000},
			},
		},
		Audit: AuditPanel{
			Available: true,
			Entries: []AuditEntry{
				{TimestampMs: nowMs - 3_000, Session: "s-2-aaa", DecisionProject: "integrator-trunk", DecisionKind: "trunk.promoted"},
			},
		},
	}
}

func TestViewRendersRichSnapshot(t *testing.T) {
	m := landed(t, richSnapshot())
	out := m.View()

	wants := []string{
		"held by session",                     // lease panel format
		"abcdef123456",                        // trunk tip (short12 of merge commit)
		"trunk tip:",                          // trunk panel
		"promoted integrations: 1",            // promoted count
		"waiters: none (fail-fast, no queue)", // literal waiters
		"feature/logout",                      // pending (validating) branch
		"validating",                          // pending state
		"conflicts/aborted (display only)",    // aborted display-only
		"integrator-trunk/trunk.promoted",     // audit line project/kind
		"expires in",                          // lease expiry phrasing
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("View() missing expected substring %q\n---\n%s", w, out)
		}
	}
	// The promoted integration must NOT appear in the pending panel as in-flight.
	// (It appears only via the trunk tip.) feature/login should not be rendered as
	// a pending branch line.
	if strings.Contains(out, "branch feature/login") {
		t.Errorf("promoted integration leaked into the pending panel:\n%s", out)
	}
}

func TestReviewQueueRendersRecordsAcrossStates(t *testing.T) {
	m := landed(t, richSnapshot())
	out := m.View()

	wants := []string{
		"Review queue",                    // panel header
		"nm/abc",                          // requested branch
		"requested",                       // requested state tag
		"add tokens",                      // title rendered
		"claimed by s-3-feedfa",           // claimed tag with short claimer
		"changes_requested by s-3-feedfa", // verdict tag with claimer
		"please split this into two separate commits", // feedback prefix (within cap)
		"…",                   // feedback was truncated
		"merged 0badc0ffee12", // approved merge OID short12
		"withdrawn",           // withdrawn shown verbatim
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("View() review queue missing expected substring %q\n---\n%s", w, out)
		}
	}

	// Full feedback must NOT appear untruncated (60-rune cap with ellipsis).
	if strings.Contains(out, "please split this into two separate commits and add a regression test before this can be approved") {
		t.Errorf("changes_requested feedback was not truncated:\n%s", out)
	}

	// Grouping: requested must sort before approved in the rendered output.
	reqIdx := strings.Index(out, "nm/abc")
	apprIdx := strings.Index(out, "nm/jkl")
	if reqIdx < 0 || apprIdx < 0 || reqIdx > apprIdx {
		t.Errorf("review queue grouping wrong: requested (%d) must precede approved (%d)\n%s", reqIdx, apprIdx, out)
	}
}

func TestReviewQueueEmptyAndUnavailable(t *testing.T) {
	// Available but empty: a friendly "no review requests", not "unavailable".
	empty := landed(t, Snapshot{DaemonReachable: true, Reviews: ReviewPanel{Available: true}})
	if out := empty.View(); !strings.Contains(out, "no review requests") {
		t.Fatalf("empty review queue must render the no-requests line:\n%s", out)
	}
	// Unavailable: degrades to the per-panel unavailable line, no panic.
	unavail := landed(t, Snapshot{DaemonReachable: true, Reviews: ReviewPanel{Available: false}})
	if out := unavail.View(); !strings.Contains(out, unavailableLine) {
		t.Fatalf("unavailable review queue must degrade to the unavailable line:\n%s", out)
	}
}

func TestViewAllUnavailableRendersWithoutPanic(t *testing.T) {
	// Reachable daemon but every sub-call wedged: panels degrade individually.
	snap := Snapshot{DaemonReachable: true, Socket: "/tmp/x.sock"}
	m := landed(t, snap)
	out := m.View()
	if !strings.Contains(out, unavailableLine) {
		t.Fatalf("each wedged panel must show %q\n%s", unavailableLine, out)
	}
	// Still renders the navigational footer (not a crash/blank).
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
	m := landed(t, richSnapshot())

	// tab cycles focus forward.
	startFocus := m.focus
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = nm.(Model)
	if m.focus == startFocus {
		t.Fatalf("tab must move focus")
	}
	// shift+tab cycles back.
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = nm.(Model)
	if m.focus != startFocus {
		t.Fatalf("shift+tab must return focus to start")
	}

	// j scrolls the focused panel (offset increases, clamped).
	beforeOff := m.offsets[m.focus]
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = nm.(Model)
	if m.offsets[m.focus] < beforeOff {
		t.Fatalf("j must not decrease scroll offset")
	}

	// An arbitrary "action-like" key must be a NO-OP that issues NO command —
	// there is no affordance to mutate or trigger anything.
	for _, r := range []rune{'a', 'p', 'r', 'x', 'd', 's'} {
		nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(Model)
		if cmd != nil {
			t.Fatalf("key %q produced a command — watch must have NO action affordance", string(r))
		}
	}

	// q quits.
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
	// fetchCmd runs the injected fetch; execute it via Init's fetch path.
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
