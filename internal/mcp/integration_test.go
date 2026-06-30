package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madhavhaldia/mad-substrate/internal/conductor"
	"github.com/madhavhaldia/mad-substrate/internal/coopclient"
)

// ----- helpers -----

// callArgs invokes a tool with arbitrary arguments and returns its single text
// block + isError.
func callArgs(t *testing.T, s *server, name string, args map[string]string) (string, bool) {
	t.Helper()
	var p toolsCallParams
	p.Name = name
	p.Arguments.Path = args["path"]
	p.Arguments.Title = args["title"]
	p.Arguments.ID = args["id"]
	p.Arguments.Feedback = args["feedback"]
	res := s.callTool(p)
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("expected one text content block, got %v", res.Content)
	}
	return res.Content[0].Text, res.IsError
}

// integratorServer builds a server in the integrator role wired to be.
func integratorServer(t *testing.T, be backend) *server {
	t.Helper()
	s := newTestServer(t, be)
	s.role = roleIntegrator
	return s
}

// gitRun runs git in dir with a hermetic, gpgsign-free environment.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// initRepoOnBranch makes a one-commit repo whose HEAD is branch.
func initRepoOnBranch(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", branch)
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "test")
	writeFile(t, dir, "f.txt", "x\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// initConvergeRepo builds a repo with a target branch and an nm/* feature branch
// one commit ahead. When conflict is true the feature and target both edit the
// SAME line so the merge conflicts; otherwise the feature only adds a new file.
// HEAD is left on the target branch (where the integrator runs).
func initConvergeRepo(t *testing.T, conflict bool) (dir, feature, target string) {
	t.Helper()
	target, feature = "trunk", "nm/s-1-feat"
	dir = t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", target)
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "test")
	writeFile(t, dir, "base.txt", "hello\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "init")

	gitRun(t, dir, "checkout", "-q", "-b", feature)
	if conflict {
		writeFile(t, dir, "base.txt", "feature-edit\n")
	} else {
		writeFile(t, dir, "feature.txt", "feature\n")
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "feat")

	gitRun(t, dir, "checkout", "-q", target)
	if conflict {
		writeFile(t, dir, "base.txt", "trunk-edit\n")
		gitRun(t, dir, "add", ".")
		gitRun(t, dir, "commit", "-q", "-m", "trunk change")
	}
	return dir, feature, target
}

// stubRPC is a fake daemon connection for conductor.Converge: it grants the
// trunk lease so the real git merge runs. It satisfies rpcConn.
type stubRPC struct{ closed bool }

func (s *stubRPC) Call(method string, _ any, out any) error {
	var canned any
	switch method {
	case "classify.route":
		canned = map[string]any{"lease_key": "trunk-lease"}
	case "lease.acquire":
		canned = map[string]any{"granted": true, "holder": "integrator"}
	default:
		return nil // lease.release etc.
	}
	if out == nil {
		return nil
	}
	b, _ := json.Marshal(canned)
	return json.Unmarshal(b, out)
}

func (s *stubRPC) Close() error { s.closed = true; return nil }

// ----- role gating -----

func toolNames(t *testing.T, role string) []string {
	t.Helper()
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	// Grant the integrator presence lease so the singleton check proceeds; this
	// helper exercises tool listing, not singleton enforcement.
	lines := runServeRole(t, &stubBackend{acquireGranted: true}, role, in)
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var names []string
	for _, tool := range resp.Result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestRoleGatingBuilder(t *testing.T) {
	names := toolNames(t, "builder")
	for _, want := range []string{"mad_request_integration", "mad_integration_status"} {
		if !has(names, want) {
			t.Errorf("builder must expose %s; got %v", want, names)
		}
	}
	for _, banned := range []string{
		"mad_integration_pending", "mad_integration_claim",
		"mad_integration_approve", "mad_integration_reject",
	} {
		if has(names, banned) {
			t.Errorf("builder must NOT expose %s; got %v", banned, names)
		}
	}
}

func TestRoleGatingIntegrator(t *testing.T) {
	names := toolNames(t, "integrator")
	for _, want := range []string{
		"mad_integration_pending", "mad_integration_claim",
		"mad_integration_approve", "mad_integration_reject",
		"mad_locks", "mad_status",
	} {
		if !has(names, want) {
			t.Errorf("integrator must expose %s; got %v", want, names)
		}
	}
	for _, banned := range []string{
		"mad_classify", "mad_claim", "mad_release",
		"mad_request_integration", "mad_integration_status",
	} {
		if has(names, banned) {
			t.Errorf("integrator must NOT expose %s; got %v", banned, names)
		}
	}
}

func TestRoleGatingDispatchRejectsWrongRole(t *testing.T) {
	// A builder cannot dispatch an integrator tool even by name.
	s := newTestServer(t, &stubBackend{}) // builder (default)
	text, isErr := callArgs(t, s, "mad_integration_approve", map[string]string{"id": "nm/x"})
	if !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("builder dispatch of integrator tool should be unknown; got %q (err=%v)", text, isErr)
	}
}

// ----- builder tools -----

func TestBuilderRequestIntegration(t *testing.T) {
	dir := initRepoOnBranch(t, "nm/s-7-mybranch")
	be := &stubBackend{reqIntID: "nm/s-7-mybranch", reqIntState: "pending"}
	s := newTestServer(t, be)
	s.getwd = func() (string, error) { return dir, nil }

	text, isErr := callArgs(t, s, "mad_request_integration", map[string]string{"title": "my work"})
	if isErr {
		t.Fatalf("request should not error: %q", text)
	}
	if be.reqIntCalls != 1 {
		t.Fatalf("expected exactly 1 request, got %d", be.reqIntCalls)
	}
	if be.reqIntBranch != "nm/s-7-mybranch" {
		t.Fatalf("branch not resolved from git HEAD: %q", be.reqIntBranch)
	}
	if be.reqIntTitle != "my work" {
		t.Fatalf("title not forwarded: %q", be.reqIntTitle)
	}
	if !strings.Contains(text, "nm/s-7-mybranch") || !strings.Contains(text, "pending") {
		t.Fatalf("render missing id/state: %q", text)
	}
}

func TestBuilderRequestIntegrationNonBoundaryBranch(t *testing.T) {
	dir := initRepoOnBranch(t, "main")
	be := &stubBackend{}
	s := newTestServer(t, be)
	s.getwd = func() (string, error) { return dir, nil }

	text, isErr := callArgs(t, s, "mad_request_integration", nil)
	if !isErr {
		t.Fatalf("non-nm branch should be a tool error")
	}
	if be.reqIntCalls != 0 {
		t.Fatalf("must not call the daemon for a non-boundary branch")
	}
	if !strings.Contains(text, "not a mad-substrate boundary branch") {
		t.Fatalf("unhelpful error: %q", text)
	}
}

func TestBuilderIntegrationStatusWithFeedback(t *testing.T) {
	dir := initRepoOnBranch(t, "nm/s-3-x")
	be := &stubBackend{statusFound: true, statusState: "changes_requested", statusFeedback: "fix the lints"}
	s := newTestServer(t, be)
	s.getwd = func() (string, error) { return dir, nil }

	text, isErr := callArgs(t, s, "mad_integration_status", nil)
	if isErr {
		t.Fatalf("status should not error: %q", text)
	}
	if be.statusBranch != "nm/s-3-x" {
		t.Fatalf("status branch not resolved: %q", be.statusBranch)
	}
	if !strings.Contains(text, "changes_requested") || !strings.Contains(text, "feedback: fix the lints") {
		t.Fatalf("status render missing state/feedback: %q", text)
	}
}

func TestBuilderIntegrationStatusNotFound(t *testing.T) {
	dir := initRepoOnBranch(t, "nm/s-3-x")
	be := &stubBackend{statusFound: false}
	s := newTestServer(t, be)
	s.getwd = func() (string, error) { return dir, nil }

	text, isErr := callArgs(t, s, "mad_integration_status", nil)
	if isErr {
		t.Fatalf("not-found status should not be a tool error: %q", text)
	}
	if !strings.Contains(text, "No integration request found") {
		t.Fatalf("got %q", text)
	}
}

// ----- integrator tools -----

func TestIntegratorPending(t *testing.T) {
	be := &stubBackend{pending: []coopclient.PendingIntegration{
		{ID: "nm/a", Branch: "nm/a", Title: "first", State: "pending"},
		{ID: "nm/b", Branch: "nm/b", State: "pending"},
	}}
	s := integratorServer(t, be)
	text, isErr := callArgs(t, s, "mad_integration_pending", nil)
	if isErr {
		t.Fatalf("pending should not error: %q", text)
	}
	if !strings.Contains(text, "nm/a") || !strings.Contains(text, `"first"`) {
		t.Fatalf("missing first request: %q", text)
	}
	if !strings.Contains(text, "nm/b") || !strings.Contains(text, "(no title)") {
		t.Fatalf("missing titleless request: %q", text)
	}
}

func TestIntegratorPendingEmpty(t *testing.T) {
	s := integratorServer(t, &stubBackend{})
	text, _ := callArgs(t, s, "mad_integration_pending", nil)
	if !strings.Contains(text, "No integration requests are pending") {
		t.Fatalf("got %q", text)
	}
}

func TestIntegratorClaim(t *testing.T) {
	be := &stubBackend{claimOK: true, claimBranch: "nm/a", claimTitle: "do a thing"}
	s := integratorServer(t, be)
	text, isErr := callArgs(t, s, "mad_integration_claim", map[string]string{"id": "nm/a"})
	if isErr {
		t.Fatalf("claim should not error: %q", text)
	}
	if !strings.Contains(text, "Claimed nm/a") || !strings.Contains(text, "do a thing") {
		t.Fatalf("got %q", text)
	}
}

func TestIntegratorClaimAlreadyTaken(t *testing.T) {
	be := &stubBackend{claimOK: false}
	s := integratorServer(t, be)
	text, isErr := callArgs(t, s, "mad_integration_claim", map[string]string{"id": "nm/a"})
	if !isErr {
		t.Fatalf("a failed claim should be a tool error")
	}
	if !strings.Contains(text, "already claimed or not pending") {
		t.Fatalf("got %q", text)
	}
}

func TestIntegratorReject(t *testing.T) {
	be := &stubBackend{verdictOK: true, verdictState: "changes_requested"}
	s := integratorServer(t, be)
	text, isErr := callArgs(t, s, "mad_integration_reject", map[string]string{"id": "nm/a", "feedback": "needs tests"})
	if isErr {
		t.Fatalf("reject should not error: %q", text)
	}
	if be.verdictCalls != 1 || be.verdictDecision != "reject" || be.verdictFeedback != "needs tests" {
		t.Fatalf("reject verdict not recorded: calls=%d decision=%q fb=%q", be.verdictCalls, be.verdictDecision, be.verdictFeedback)
	}
	if !strings.Contains(text, "Rejected nm/a") {
		t.Fatalf("got %q", text)
	}
}

func TestIntegratorRejectRequiresFeedback(t *testing.T) {
	be := &stubBackend{}
	s := integratorServer(t, be)
	text, isErr := callArgs(t, s, "mad_integration_reject", map[string]string{"id": "nm/a", "feedback": "  "})
	if !isErr {
		t.Fatalf("empty feedback must be a tool error")
	}
	if be.verdictCalls != 0 {
		t.Fatalf("must not record a verdict without feedback")
	}
	if !strings.Contains(text, "feedback is required") {
		t.Fatalf("got %q", text)
	}
}

// ----- the gated merge (approve) -----

func TestIntegratorApproveConverged(t *testing.T) {
	dir, feature, target := initConvergeRepo(t, false)
	before := gitRun(t, dir, "rev-parse", "HEAD")

	be := &stubBackend{verdictOK: true, verdictState: "merged"}
	s := integratorServer(t, be)
	s.getwd = func() (string, error) { return dir, nil }
	s.dialRPC = func() (rpcConn, error) { return &stubRPC{}, nil }
	// converge left nil -> real conductor.Converge runs the real git merge.

	text, isErr := callArgs(t, s, "mad_integration_approve", map[string]string{"id": feature})
	if isErr {
		t.Fatalf("clean merge should converge: %q", text)
	}
	if !strings.Contains(text, "merged "+feature+" into "+target) {
		t.Fatalf("approve render wrong: %q", text)
	}
	if be.verdictCalls != 1 {
		t.Fatalf("converged approve must record exactly one verdict, got %d", be.verdictCalls)
	}
	if be.verdictDecision != "approve" {
		t.Fatalf("decision must be approve, got %q", be.verdictDecision)
	}
	if be.verdictMerge == "" {
		t.Fatalf("verdict must carry the merge OID")
	}
	after := gitRun(t, dir, "rev-parse", "HEAD")
	if after == before {
		t.Fatalf("target branch did not advance (before==after==%s)", after)
	}
	if be.verdictMerge != after {
		t.Fatalf("recorded merge OID %q != new HEAD %q", be.verdictMerge, after)
	}
}

func TestIntegratorApproveConflictNoVerdict(t *testing.T) {
	dir, feature, _ := initConvergeRepo(t, true)
	before := gitRun(t, dir, "rev-parse", "HEAD")

	be := &stubBackend{}
	s := integratorServer(t, be)
	s.getwd = func() (string, error) { return dir, nil }
	s.dialRPC = func() (rpcConn, error) { return &stubRPC{}, nil }

	text, isErr := callArgs(t, s, "mad_integration_approve", map[string]string{"id": feature})
	if !isErr {
		t.Fatalf("a conflicting merge must be a tool error")
	}
	if be.verdictCalls != 0 {
		t.Fatalf("a non-converged approve must NOT record a verdict, got %d", be.verdictCalls)
	}
	if !strings.Contains(text, "Did NOT approve") {
		t.Fatalf("got %q", text)
	}
	after := gitRun(t, dir, "rev-parse", "HEAD")
	if after != before {
		t.Fatalf("conflicting merge must leave target untouched (before=%s after=%s)", before, after)
	}
}

// ----- opt-in gate-on-approve (R12) -----

// TestIntegratorApproveGateEnvPlumbed proves R12: with MAD_INTEGRATOR_GATE
// set, the approve handler passes that command as Spec.Gate (run in the trunk
// worktree, BoundaryDir==repoDir) into Converge.
func TestIntegratorApproveGateEnvPlumbed(t *testing.T) {
	t.Setenv("MAD_INTEGRATOR_GATE", "make conform")
	dir, feature, _ := initConvergeRepo(t, false)

	be := &stubBackend{verdictOK: true, verdictState: "merged"}
	s := integratorServer(t, be)
	s.getwd = func() (string, error) { return dir, nil }
	s.dialRPC = func() (rpcConn, error) { return &stubRPC{}, nil }

	var gotGate, gotBoundary string
	s.converge = func(_ conductor.RPCClient, spec conductor.Spec) conductor.Result {
		gotGate, gotBoundary = spec.Gate, spec.BoundaryDir
		return conductor.Result{Status: conductor.StatusConverged, Merge: "deadbeef"}
	}

	text, isErr := callArgs(t, s, "mad_integration_approve", map[string]string{"id": feature})
	if isErr {
		t.Fatalf("a converged approve should not error: %q", text)
	}
	if gotGate != "make conform" {
		t.Fatalf("approve must plumb MAD_INTEGRATOR_GATE into Spec.Gate, got %q", gotGate)
	}
	if gotBoundary != dir {
		t.Fatalf("the gate must run in the trunk worktree (BoundaryDir==repoDir), got %q", gotBoundary)
	}
}

// TestIntegratorApproveGateUnsetMergeOnly is the NON-VACUOUS control: with the
// gate env empty/unset the Spec.Gate is "" — byte-identical to today's merge-only
// approve.
func TestIntegratorApproveGateUnsetMergeOnly(t *testing.T) {
	t.Setenv("MAD_INTEGRATOR_GATE", "") // empty ⇒ merge-only
	dir, feature, _ := initConvergeRepo(t, false)

	be := &stubBackend{verdictOK: true, verdictState: "merged"}
	s := integratorServer(t, be)
	s.getwd = func() (string, error) { return dir, nil }
	s.dialRPC = func() (rpcConn, error) { return &stubRPC{}, nil }

	var gotGate string
	s.converge = func(_ conductor.RPCClient, spec conductor.Spec) conductor.Result {
		gotGate = spec.Gate
		return conductor.Result{Status: conductor.StatusConverged, Merge: "deadbeef"}
	}

	if _, isErr := callArgs(t, s, "mad_integration_approve", map[string]string{"id": feature}); isErr {
		t.Fatalf("merge-only approve should not error")
	}
	if gotGate != "" {
		t.Fatalf("with no gate env, Spec.Gate must be empty (merge-only), got %q", gotGate)
	}
}

// TestIntegratorApproveGateFailedNoVerdict proves the gate-failure path surfaces
// clearly: a StatusGateFailed convergence reports "Did NOT approve" and records
// NO verdict, leaving the integrator to resolve manually or reject.
func TestIntegratorApproveGateFailedNoVerdict(t *testing.T) {
	t.Setenv("MAD_INTEGRATOR_GATE", "exit 1")
	dir, feature, _ := initConvergeRepo(t, false)

	be := &stubBackend{}
	s := integratorServer(t, be)
	s.getwd = func() (string, error) { return dir, nil }
	s.dialRPC = func() (rpcConn, error) { return &stubRPC{}, nil }
	s.converge = func(_ conductor.RPCClient, spec conductor.Spec) conductor.Result {
		if spec.Gate != "exit 1" {
			t.Fatalf("gate not plumbed into Spec.Gate: %q", spec.Gate)
		}
		return conductor.Result{Status: conductor.StatusGateFailed, Reason: "gate command failed: exit status 1"}
	}

	text, isErr := callArgs(t, s, "mad_integration_approve", map[string]string{"id": feature})
	if !isErr {
		t.Fatalf("a gate failure must be a tool error")
	}
	if be.verdictCalls != 0 {
		t.Fatalf("a gate failure must record NO verdict, got %d", be.verdictCalls)
	}
	if !strings.Contains(text, "Did NOT approve") || !strings.Contains(text, string(conductor.StatusGateFailed)) {
		t.Fatalf("gate failure must surface clearly: %q", text)
	}
}

func TestIntegratorApproveUnsafeRef(t *testing.T) {
	be := &stubBackend{}
	s := integratorServer(t, be)
	called := false
	s.dialRPC = func() (rpcConn, error) { called = true; return &stubRPC{}, nil }
	text, isErr := callArgs(t, s, "mad_integration_approve", map[string]string{"id": "--upload-pack=evil"})
	if !isErr {
		t.Fatalf("unsafe ref must be a tool error")
	}
	if called {
		t.Fatalf("must not dial for an unsafe ref")
	}
	if !strings.Contains(text, "unsafe branch ref") {
		t.Fatalf("got %q", text)
	}
}
