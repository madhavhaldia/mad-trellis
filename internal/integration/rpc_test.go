package integration

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
)

// newTestIntegration builds an Integration over a temp on-disk ledger.
func newTestIntegration(t *testing.T) *Integration {
	t.Helper()
	ig, err := New(Options{StorePath: filepath.Join(t.TempDir(), "integration.db")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = ig.Close() })
	return ig
}

// ccFor fabricates a CallContext with a distinct connection-bound session (the
// only identity the handlers read — never params).
func ccFor(sess string) *daemon.CallContext {
	return &daemon.CallContext{Session: daemon.SessionID(sess)}
}

func mustParams(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return b
}

// TestHandlersDriveFullLoop drives all five methods through fake CallContexts:
// a BUILDER session requests; an INTEGRATOR session lists, claims, and verdicts;
// the builder polls status. It then exercises both verdicts and the re-request
// reset across two distinct branches.
func TestHandlersDriveFullLoop(t *testing.T) {
	ig := newTestIntegration(t)
	builder := ccFor("s-builder")
	integr := ccFor("s-integrator")

	// 1) builder requests integration of its boundary branch.
	raw, perr := requestHandler(ig)(builder, mustParams(t, map[string]any{"branch": "feature/login", "title": "Login flow"}))
	if perr != nil {
		t.Fatalf("request: %+v", perr)
	}
	var reqOut struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	mustJSON(t, raw, &reqOut)
	if reqOut.ID != "feature/login" || reqOut.State != "requested" {
		t.Fatalf("request result: %+v", reqOut)
	}
	// Holder must be the builder's connection session, never from params.
	if rec, _, _ := ig.Status("feature/login"); rec.Holder != "s-builder" {
		t.Fatalf("holder must be cc.Session (s-builder); got %q", rec.Holder)
	}

	// 2) integrator lists pending — sees the one request.
	raw, perr = pendingHandler(ig)(integr, nil)
	if perr != nil {
		t.Fatalf("pending: %+v", perr)
	}
	var pendOut struct {
		Pending []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			State string `json:"state"`
		} `json:"pending"`
	}
	mustJSON(t, raw, &pendOut)
	if len(pendOut.Pending) != 1 || pendOut.Pending[0].ID != "feature/login" || pendOut.Pending[0].Title != "Login flow" {
		t.Fatalf("pending result: %+v", pendOut)
	}

	// 3) integrator claims it.
	raw, perr = claimHandler(ig)(integr, mustParams(t, map[string]any{"id": "feature/login"}))
	if perr != nil {
		t.Fatalf("claim: %+v", perr)
	}
	var claimOut struct {
		OK     bool   `json:"ok"`
		Branch string `json:"branch"`
		Title  string `json:"title"`
	}
	mustJSON(t, raw, &claimOut)
	if !claimOut.OK || claimOut.Branch != "feature/login" || claimOut.Title != "Login flow" {
		t.Fatalf("claim result: %+v", claimOut)
	}
	if rec, _, _ := ig.Status("feature/login"); rec.Claimer != "s-integrator" {
		t.Fatalf("claimer must be cc.Session (s-integrator); got %q", rec.Claimer)
	}

	// A second claim (lost race) returns ok=false, not an error.
	raw, perr = claimHandler(ig)(ccFor("s-integrator-2"), mustParams(t, map[string]any{"id": "feature/login"}))
	if perr != nil {
		t.Fatalf("second claim: %+v", perr)
	}
	mustJSON(t, raw, &claimOut)
	if claimOut.OK {
		t.Fatal("a second claim of a claimed request must return ok=false")
	}

	// 4) integrator approves, passing the merge OID it already produced.
	raw, perr = verdictHandler(ig)(integr, mustParams(t, map[string]any{"id": "feature/login", "decision": "approve", "merge": "abc123"}))
	if perr != nil {
		t.Fatalf("verdict approve: %+v", perr)
	}
	var verdOut struct {
		OK    bool   `json:"ok"`
		State string `json:"state"`
	}
	mustJSON(t, raw, &verdOut)
	if !verdOut.OK || verdOut.State != "approved" {
		t.Fatalf("approve result: %+v", verdOut)
	}

	// 5) builder polls status — sees approved + the merge OID.
	raw, perr = statusHandler(ig)(builder, mustParams(t, map[string]any{"branch": "feature/login"}))
	if perr != nil {
		t.Fatalf("status: %+v", perr)
	}
	var statOut struct {
		Found    bool   `json:"found"`
		ID       string `json:"id"`
		State    string `json:"state"`
		Feedback string `json:"feedback"`
		Merge    string `json:"merge"`
	}
	mustJSON(t, raw, &statOut)
	if !statOut.Found || statOut.State != "approved" || statOut.Merge != "abc123" {
		t.Fatalf("status result: %+v", statOut)
	}
}

// TestVerdictRejectRequiresFeedbackAndResets covers the reject path + invalid
// params + the builder re-requesting after changes_requested.
func TestVerdictRejectRequiresFeedbackAndResets(t *testing.T) {
	ig := newTestIntegration(t)
	builder := ccFor("s-builder")
	integr := ccFor("s-integrator")

	if _, perr := requestHandler(ig)(builder, mustParams(t, map[string]any{"branch": "feature/api"})); perr != nil {
		t.Fatalf("request: %+v", perr)
	}
	if _, perr := claimHandler(ig)(integr, mustParams(t, map[string]any{"id": "feature/api"})); perr != nil {
		t.Fatalf("claim: %+v", perr)
	}

	// reject WITHOUT feedback → CodeInvalidParams.
	_, perr := verdictHandler(ig)(integr, mustParams(t, map[string]any{"id": "feature/api", "decision": "reject"}))
	if perr == nil || perr.Code != -32602 {
		t.Fatalf("reject without feedback must be CodeInvalidParams; got %+v", perr)
	}

	// reject WITH feedback → changes_requested.
	raw, perr := verdictHandler(ig)(integr, mustParams(t, map[string]any{"id": "feature/api", "decision": "reject", "feedback": "add error handling"}))
	if perr != nil {
		t.Fatalf("reject: %+v", perr)
	}
	var verdOut struct {
		OK    bool   `json:"ok"`
		State string `json:"state"`
	}
	mustJSON(t, raw, &verdOut)
	if !verdOut.OK || verdOut.State != "changes_requested" {
		t.Fatalf("reject result: %+v", verdOut)
	}

	// builder sees the feedback via status.
	raw, _ = statusHandler(ig)(builder, mustParams(t, map[string]any{"branch": "feature/api"}))
	var statOut struct {
		State    string `json:"state"`
		Feedback string `json:"feedback"`
	}
	mustJSON(t, raw, &statOut)
	if statOut.State != "changes_requested" || statOut.Feedback != "add error handling" {
		t.Fatalf("status after reject: %+v", statOut)
	}

	// builder revises and re-requests → back to requested, feedback cleared.
	raw, perr = requestHandler(ig)(builder, mustParams(t, map[string]any{"branch": "feature/api"}))
	if perr != nil {
		t.Fatalf("re-request: %+v", perr)
	}
	var reqOut struct {
		State string `json:"state"`
	}
	mustJSON(t, raw, &reqOut)
	if reqOut.State != "requested" {
		t.Fatalf("re-request must reset to requested; got %q", reqOut.State)
	}
	if rec, _, _ := ig.Status("feature/api"); rec.Feedback != "" || rec.Claimer != "" {
		t.Fatalf("re-request must clear feedback+claimer; got %+v", rec)
	}
}

// TestRequestRejectsUnsafeBranch proves an option-like / metacharacter branch is
// refused with CodeInvalidParams.
func TestRequestRejectsUnsafeBranch(t *testing.T) {
	ig := newTestIntegration(t)
	for _, bad := range []string{"-x", "--upload-pack=evil", "a b", "a^b", "a:b", ""} {
		_, perr := requestHandler(ig)(ccFor("s-builder"), mustParams(t, map[string]any{"branch": bad}))
		if perr == nil || perr.Code != -32602 {
			t.Fatalf("unsafe branch %q must be CodeInvalidParams; got %+v", bad, perr)
		}
	}
}

// TestVerdictWrongStateNoOp proves a verdict whose CAS predicate doesn't hold
// (the row was never claimed) returns ok=false, not an error.
func TestVerdictWrongStateNoOp(t *testing.T) {
	ig := newTestIntegration(t)
	if _, perr := requestHandler(ig)(ccFor("s-builder"), mustParams(t, map[string]any{"branch": "feature/q"})); perr != nil {
		t.Fatalf("request: %+v", perr)
	}
	// approve a requested (un-claimed) row.
	raw, perr := verdictHandler(ig)(ccFor("s-integrator"), mustParams(t, map[string]any{"id": "feature/q", "decision": "approve", "merge": "x"}))
	if perr != nil {
		t.Fatalf("verdict: %+v", perr)
	}
	var verdOut struct {
		OK    bool   `json:"ok"`
		State string `json:"state"`
	}
	mustJSON(t, raw, &verdOut)
	if verdOut.OK {
		t.Fatal("approve of an un-claimed request must be ok=false")
	}
	if verdOut.State != "requested" {
		t.Fatalf("failed verdict must report current state requested; got %q", verdOut.State)
	}
}

// TestCancelHandler drives integration.cancel: a requested row is withdrawn
// (ok=true), a missing id is CodeInvalidParams, and an absent branch is ok=false.
func TestCancelHandler(t *testing.T) {
	ig := newTestIntegration(t)
	builder := ccFor("s-builder")

	if _, perr := requestHandler(ig)(builder, mustParams(t, map[string]any{"branch": "feature/c"})); perr != nil {
		t.Fatalf("request: %+v", perr)
	}

	// missing id → CodeInvalidParams.
	if _, perr := cancelHandler(ig)(builder, mustParams(t, map[string]any{})); perr == nil || perr.Code != -32602 {
		t.Fatalf("cancel without id must be CodeInvalidParams; got %+v", perr)
	}

	// cancel the in-flight request → ok=true, state withdrawn.
	raw, perr := cancelHandler(ig)(builder, mustParams(t, map[string]any{"id": "feature/c"}))
	if perr != nil {
		t.Fatalf("cancel: %+v", perr)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	mustJSON(t, raw, &out)
	if !out.OK {
		t.Fatal("cancel of a requested row must return ok=true")
	}
	if rec, _, _ := ig.Status("feature/c"); rec.State != StateWithdrawn {
		t.Fatalf("cancel must withdraw; got %q", rec.State)
	}

	// cancel an absent branch → ok=false (not an error).
	raw, perr = cancelHandler(ig)(builder, mustParams(t, map[string]any{"id": "nope"}))
	if perr != nil {
		t.Fatalf("cancel absent: %+v", perr)
	}
	mustJSON(t, raw, &out)
	if out.OK {
		t.Fatal("cancel of an absent branch must return ok=false")
	}
}

// TestListHandler proves integration.list returns ALL records in any state with
// the full field set, and is READ-ONLY (it mutates no state).
func TestListHandler(t *testing.T) {
	ig := newTestIntegration(t)
	builder := ccFor("s-builder")
	integr := ccFor("s-integrator")

	_, _ = requestHandler(ig)(builder, mustParams(t, map[string]any{"branch": "br-a", "title": "A"}))
	_, _ = requestHandler(ig)(builder, mustParams(t, map[string]any{"branch": "br-b", "title": "B"}))
	_, _ = claimHandler(ig)(integr, mustParams(t, map[string]any{"id": "br-b"}))

	raw, perr := listHandler(ig)(integr, nil)
	if perr != nil {
		t.Fatalf("list: %+v", perr)
	}
	var out struct {
		Records []struct {
			ID      string `json:"id"`
			Branch  string `json:"branch"`
			Title   string `json:"title"`
			State   string `json:"state"`
			Claimer string `json:"claimer"`
		} `json:"records"`
	}
	mustJSON(t, raw, &out)
	if len(out.Records) != 2 {
		t.Fatalf("list must return ALL records regardless of state; got %d", len(out.Records))
	}
	byID := map[string]string{}    // id -> state
	claimer := map[string]string{} // id -> claimer
	for _, r := range out.Records {
		byID[r.ID] = r.State
		claimer[r.ID] = r.Claimer
	}
	if byID["br-a"] != "requested" || byID["br-b"] != "claimed" {
		t.Fatalf("list must report each row's true state; got %v", byID)
	}
	if claimer["br-b"] != "s-integrator" {
		t.Fatalf("list must surface the claimer; got %q", claimer["br-b"])
	}

	// READ-ONLY: a second list returns the SAME states (no mutation occurred).
	raw, _ = listHandler(ig)(integr, nil)
	mustJSON(t, raw, &out)
	for _, r := range out.Records {
		if r.ID == "br-b" && r.State != "claimed" {
			t.Fatalf("list must not mutate state; br-b became %q", r.State)
		}
	}
}

func mustJSON(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
}
