package integration

import (
	"path/filepath"
	"testing"
)

// newTestStore opens a fresh on-disk store under a temp ledger file (mirrors the
// integrator package's use of a real SQLite file per test).
func newTestStore(t *testing.T) *store {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "integration.db"), nil)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.close() })
	return st
}

// TestRequestClaimApprove walks the happy path requested -> claimed -> approved.
func TestRequestClaimApprove(t *testing.T) {
	st := newTestStore(t)

	rec, err := st.upsertRequest("feature/x", "Add X", "builder-1")
	if err != nil {
		t.Fatalf("upsertRequest: %v", err)
	}
	if rec.State != StateRequested || rec.Holder != "builder-1" || rec.Branch != "feature/x" {
		t.Fatalf("request landed wrong: %+v", rec)
	}

	ok, err := st.claim("feature/x", "integrator-1")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	got, _, _ := st.get("feature/x")
	if got.State != StateClaimed || got.Claimer != "integrator-1" {
		t.Fatalf("after claim: %+v", got)
	}

	ok, err = st.approve("feature/x", "deadbeef")
	if err != nil || !ok {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	got, _, _ = st.get("feature/x")
	if got.State != StateApproved || got.Merge != "deadbeef" {
		t.Fatalf("after approve: %+v", got)
	}
}

// TestRequestClaimReject walks requested -> claimed -> changes_requested and
// checks feedback is stored.
func TestRequestClaimReject(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.upsertRequest("feature/y", "", "builder-1"); err != nil {
		t.Fatalf("upsertRequest: %v", err)
	}
	if ok, err := st.claim("feature/y", "integrator-1"); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	ok, err := st.reject("feature/y", "needs tests")
	if err != nil || !ok {
		t.Fatalf("reject: ok=%v err=%v", ok, err)
	}
	got, _, _ := st.get("feature/y")
	if got.State != StateChangesRequested || got.Feedback != "needs tests" {
		t.Fatalf("after reject: %+v", got)
	}
}

// TestReRequestResets proves a builder re-requesting after revising RESETS the
// row to requested and clears the prior claimer + feedback, while preserving
// created_at (one row per branch).
func TestReRequestResets(t *testing.T) {
	st := newTestStore(t)
	first, _ := st.upsertRequest("feature/z", "v1", "builder-1")
	_, _ = st.claim("feature/z", "integrator-1")
	_, _ = st.reject("feature/z", "fix the bug")

	// re-request (the builder revised)
	again, err := st.upsertRequest("feature/z", "", "builder-1")
	if err != nil {
		t.Fatalf("re-request: %v", err)
	}
	if again.State != StateRequested {
		t.Fatalf("re-request must reset to requested; got %q", again.State)
	}
	got, _, _ := st.get("feature/z")
	if got.State != StateRequested {
		t.Fatalf("state after re-request: %q", got.State)
	}
	if got.Claimer != "" {
		t.Fatalf("re-request must clear claimer; got %q", got.Claimer)
	}
	if got.Feedback != "" {
		t.Fatalf("re-request must clear feedback; got %q", got.Feedback)
	}
	if got.Title != "v1" {
		t.Fatalf("empty re-request title must preserve prior title; got %q", got.Title)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("re-request must preserve created_at; first=%v again=%v", first.CreatedAt, got.CreatedAt)
	}

	// Re-requesting an APPROVED branch likewise re-opens it as requested.
	st2 := newTestStore(t)
	_, _ = st2.upsertRequest("feature/a", "", "b")
	_, _ = st2.claim("feature/a", "i")
	_, _ = st2.approve("feature/a", "cafef00d")
	reopened, _ := st2.upsertRequest("feature/a", "", "b")
	if reopened.State != StateRequested {
		t.Fatalf("re-request of approved must re-open to requested; got %q", reopened.State)
	}
	g2, _, _ := st2.get("feature/a")
	if g2.Merge != "" {
		t.Fatalf("re-request must clear stale merge oid; got %q", g2.Merge)
	}
}

// TestPendingListsOnlyRequested proves pending() returns only `requested` rows,
// oldest first, and drops a row once it is claimed.
func TestPendingListsOnlyRequested(t *testing.T) {
	st := newTestStore(t)
	_, _ = st.upsertRequest("br-1", "one", "b")
	_, _ = st.upsertRequest("br-2", "two", "b")
	_, _ = st.upsertRequest("br-3", "three", "b")

	// claim br-2 → it leaves the pending queue.
	if ok, err := st.claim("br-2", "i"); err != nil || !ok {
		t.Fatalf("claim br-2: ok=%v err=%v", ok, err)
	}

	pend, err := st.pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pend) != 2 {
		t.Fatalf("pending must list only requested rows; got %d: %+v", len(pend), pend)
	}
	if pend[0].Branch != "br-1" || pend[1].Branch != "br-3" {
		t.Fatalf("pending must be oldest-first and exclude claimed; got %v, %v", pend[0].Branch, pend[1].Branch)
	}
}

// TestWrongStateCASNoOp proves a CAS whose state predicate doesn't hold returns
// ok=false (not an error) and mutates nothing.
func TestWrongStateCASNoOp(t *testing.T) {
	st := newTestStore(t)
	_, _ = st.upsertRequest("br", "", "b")

	// claim before request? approve/reject on a `requested` (not claimed) row.
	if ok, err := st.approve("br", "x"); err != nil || ok {
		t.Fatalf("approve on requested must be ok=false no-op; ok=%v err=%v", ok, err)
	}
	if ok, err := st.reject("br", "fb"); err != nil || ok {
		t.Fatalf("reject on requested must be ok=false no-op; ok=%v err=%v", ok, err)
	}
	got, _, _ := st.get("br")
	if got.State != StateRequested {
		t.Fatalf("failed CAS must not mutate state; got %q", got.State)
	}

	// double claim: first wins, second is a no-op.
	if ok, _ := st.claim("br", "i1"); !ok {
		t.Fatal("first claim must win")
	}
	if ok, err := st.claim("br", "i2"); err != nil || ok {
		t.Fatalf("second claim must be ok=false no-op; ok=%v err=%v", ok, err)
	}
	got, _, _ = st.get("br")
	if got.Claimer != "i1" {
		t.Fatalf("a lost claim must not overwrite the claimer; got %q", got.Claimer)
	}

	// claim of an unknown branch is a no-op (no row).
	if ok, err := st.claim("nope", "i"); err != nil || ok {
		t.Fatalf("claim of unknown branch must be ok=false; ok=%v err=%v", ok, err)
	}
}

// TestCancelWithdrawsInFlight proves cancel withdraws requested + claimed rows,
// is ok=false (a no-op) on a terminal/verdict row or an absent branch, and never
// clobbers a landed approve.
func TestCancelWithdrawsInFlight(t *testing.T) {
	st := newTestStore(t)

	// requested -> withdrawn.
	_, _ = st.upsertRequest("br-req", "", "b")
	if ok, err := st.cancel("br-req"); err != nil || !ok {
		t.Fatalf("cancel of requested must be ok=true; ok=%v err=%v", ok, err)
	}
	if got, _, _ := st.get("br-req"); got.State != StateWithdrawn {
		t.Fatalf("cancel must withdraw; got %q", got.State)
	}

	// claimed -> withdrawn.
	_, _ = st.upsertRequest("br-claim", "", "b")
	_, _ = st.claim("br-claim", "i")
	if ok, err := st.cancel("br-claim"); err != nil || !ok {
		t.Fatalf("cancel of claimed must be ok=true; ok=%v err=%v", ok, err)
	}
	if got, _, _ := st.get("br-claim"); got.State != StateWithdrawn {
		t.Fatalf("cancel must withdraw a claimed row; got %q", got.State)
	}

	// approved -> ok=false (must NOT clobber a landed verdict).
	_, _ = st.upsertRequest("br-appr", "", "b")
	_, _ = st.claim("br-appr", "i")
	_, _ = st.approve("br-appr", "deadbeef")
	if ok, err := st.cancel("br-appr"); err != nil || ok {
		t.Fatalf("cancel of approved must be ok=false no-op; ok=%v err=%v", ok, err)
	}
	if got, _, _ := st.get("br-appr"); got.State != StateApproved || got.Merge != "deadbeef" {
		t.Fatalf("cancel must not clobber an approved row; got %+v", got)
	}

	// absent -> ok=false.
	if ok, err := st.cancel("nope"); err != nil || ok {
		t.Fatalf("cancel of absent branch must be ok=false; ok=%v err=%v", ok, err)
	}
}

// TestReclaimStaleClaims proves a claimed row with a DEAD claimer reverts to
// requested (claimer + feedback cleared), a claimed row with a LIVE claimer is
// untouched, and non-claimed rows are never touched.
func TestReclaimStaleClaims(t *testing.T) {
	st := newTestStore(t)

	// a claimed row whose claimer is DEAD.
	_, _ = st.upsertRequest("br-dead", "", "b")
	_, _ = st.claim("br-dead", "dead-integrator")

	// a claimed row whose claimer is LIVE.
	_, _ = st.upsertRequest("br-live", "", "b")
	_, _ = st.claim("br-live", "live-integrator")

	// a requested (non-claimed) row and an approved one — neither must be touched.
	_, _ = st.upsertRequest("br-requested", "", "b")
	_, _ = st.upsertRequest("br-approved", "", "b")
	_, _ = st.claim("br-approved", "live-integrator")
	_, _ = st.approve("br-approved", "cafe")

	dead := map[string]bool{"dead-integrator": true}
	n, err := st.reclaimStaleClaims(func(s string) bool { return dead[s] })
	if err != nil {
		t.Fatalf("reclaimStaleClaims: %v", err)
	}
	if n != 1 {
		t.Fatalf("only the dead claimer's row must be reclaimed; got %d", n)
	}

	if got, _, _ := st.get("br-dead"); got.State != StateRequested || got.Claimer != "" {
		t.Fatalf("a dead claimer's row must revert to requested + clear claimer; got %+v", got)
	}
	if got, _, _ := st.get("br-live"); got.State != StateClaimed || got.Claimer != "live-integrator" {
		t.Fatalf("a LIVE claimer's row must be untouched; got %+v", got)
	}
	if got, _, _ := st.get("br-requested"); got.State != StateRequested {
		t.Fatalf("a requested row must be untouched; got %q", got.State)
	}
	if got, _, _ := st.get("br-approved"); got.State != StateApproved {
		t.Fatalf("an approved row must be untouched; got %q", got.State)
	}
}

// TestReclaimStaleClaimsClearsFeedback proves the revert also clears any prior
// feedback (a stale rejection note must not survive the reclaim).
func TestReclaimStaleClaimsClearsFeedback(t *testing.T) {
	st := newTestStore(t)
	// Manufacture a claimed row that also carries feedback: request, claim, reject
	// (sets feedback + changes_requested), re-request (back to requested, clears),
	// then claim again — feedback is "" here, so seed it directly to be sure.
	_, _ = st.upsertRequest("br", "", "b")
	_, _ = st.claim("br", "dead")
	// Directly seed feedback on the claimed row (a real reject would leave claimed).
	if _, err := st.db.Exec(`UPDATE integration_requests SET feedback = ? WHERE branch = ?`, "stale note", "br"); err != nil {
		t.Fatalf("seed feedback: %v", err)
	}
	n, err := st.reclaimStaleClaims(func(string) bool { return true })
	if err != nil || n != 1 {
		t.Fatalf("reclaim: n=%d err=%v", n, err)
	}
	if got, _, _ := st.get("br"); got.Feedback != "" {
		t.Fatalf("reclaim must clear feedback; got %q", got.Feedback)
	}
}

// TestListReturnsAllStates proves list() returns rows in EVERY state (not just
// requested), newest-updated first.
func TestListReturnsAllStates(t *testing.T) {
	st := newTestStore(t)
	_, _ = st.upsertRequest("br-req", "", "b")   // requested
	_, _ = st.upsertRequest("br-claim", "", "b") // claimed
	_, _ = st.claim("br-claim", "i")
	_, _ = st.upsertRequest("br-appr", "", "b") // approved
	_, _ = st.claim("br-appr", "i")
	_, _ = st.approve("br-appr", "x")
	_, _ = st.upsertRequest("br-wd", "", "b") // withdrawn
	_, _ = st.cancel("br-wd")

	recs, err := st.list()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("list must return ALL rows regardless of state; got %d", len(recs))
	}
	seen := map[State]bool{}
	for _, r := range recs {
		seen[r.State] = true
	}
	for _, want := range []State{StateRequested, StateClaimed, StateApproved, StateWithdrawn} {
		if !seen[want] {
			t.Fatalf("list must include a %q row; saw %v", want, seen)
		}
	}
}
