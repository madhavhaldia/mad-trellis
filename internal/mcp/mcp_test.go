package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/coopclient"
	"github.com/madhavhaldia/mad-substrate/internal/manifest"
)

// transportError is a transport-class *coopclient.Error (Code 0) so the stub can
// drive the fail-soft / re-Dial paths. coopclient.IsTransport keys off Code==0.
func transportError(msg string) error { return &coopclient.Error{Code: 0, Message: msg} }

// verdictError is a non-transport daemon verdict (Code != 0) so the stub can
// drive the "surface the verdict" branch (not the fail-soft note).
func verdictError(msg string) error { return &coopclient.Error{Code: -32003, Message: msg} }

// stubBackend is a hand-driven backend implementing the daemon surface so the
// handlers are exercised with zero live daemon. Every method consults its
// configured fields; counters let tests assert re-Dial / call counts.
type stubBackend struct {
	mu sync.Mutex

	holder string

	routeKind  string
	routeKey   string
	routeErr   error
	routeErrFn func(name string) error // optional per-call override

	acquireGranted bool
	acquireHolder  string
	acquireErr     error
	// acquireByKey, when non-nil, overrides acquireGranted per lease key: a key
	// present-and-true is granted, otherwise denied (held by acquireHolder). Lets
	// the integrator-pool tests simulate specific slots being free or occupied.
	acquireByKey map[string]bool

	renewOK  bool
	renewErr error

	releaseOK  bool
	releaseErr error

	holders    []coopclient.Holder
	holdersErr error

	ints    []coopclient.Integration
	intsErr error

	// integration plane — request (builder)
	reqIntID, reqIntState     string
	reqIntErr                 error
	reqIntBranch, reqIntTitle string // recorded

	// integration plane — status (builder)
	statusFound                            bool
	statusState, statusFeedback, statusMrg string
	statusErr                              error
	statusBranch                           string // recorded

	// integration plane — pending/claim (integrator)
	pending                 []coopclient.PendingIntegration
	pendingErr              error
	claimOK                 bool
	claimBranch, claimTitle string
	claimErr                error

	// integration plane — verdict (integrator)
	verdictOK                                                 bool
	verdictState                                              string
	verdictErr                                                error
	verdictID, verdictDecision, verdictFeedback, verdictMerge string // recorded

	// integration event inbox
	events       []coopclient.IntegrationEvent
	eventsErr    error
	eventsNotify chan struct{}
	eventsOnce   sync.Once
	eventsCalls  int
	eventsBranch string
	eventsMax    int

	// read-only lease inspect
	leaseViews        map[string]coopclient.LeaseView // raw lease key -> view
	leaseInspectErr   error
	leaseInspectCalls []string // raw lease keys

	// counters
	renewCalls   int
	releaseCalls int
	closeCalls   int
	routeCalls   int
	reqIntCalls  int
	verdictCalls int
}

func (s *stubBackend) RequestIntegration(branch, title string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqIntCalls++
	s.reqIntBranch, s.reqIntTitle = branch, title
	return s.reqIntID, s.reqIntState, s.reqIntErr
}

func (s *stubBackend) IntegrationStatus(branch string) (bool, string, string, string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusBranch = branch
	return s.statusFound, "", s.statusState, s.statusFeedback, s.statusMrg, s.statusErr
}

func (s *stubBackend) IntegrationPending() ([]coopclient.PendingIntegration, error) {
	return s.pending, s.pendingErr
}

func (s *stubBackend) IntegrationClaim(id string) (bool, string, string, error) {
	return s.claimOK, s.claimBranch, s.claimTitle, s.claimErr
}

func (s *stubBackend) IntegrationVerdict(id, decision, feedback, merge string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verdictCalls++
	s.verdictID, s.verdictDecision, s.verdictFeedback, s.verdictMerge = id, decision, feedback, merge
	return s.verdictOK, s.verdictState, s.verdictErr
}

func (s *stubBackend) IntegrationEvents(branch string, max int) ([]coopclient.IntegrationEvent, error) {
	s.mu.Lock()
	s.eventsCalls++
	s.eventsBranch = branch
	s.eventsMax = max
	if s.eventsNotify != nil {
		s.eventsOnce.Do(func() { close(s.eventsNotify) })
	}
	if s.eventsErr != nil {
		err := s.eventsErr
		s.mu.Unlock()
		return nil, err
	}
	out := append([]coopclient.IntegrationEvent(nil), s.events...)
	s.events = nil
	s.mu.Unlock()
	return out, nil
}

func (s *stubBackend) LeaseInspect(key []byte) (coopclient.LeaseView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw := string(key)
	s.leaseInspectCalls = append(s.leaseInspectCalls, raw)
	if s.leaseInspectErr != nil {
		return coopclient.LeaseView{}, s.leaseInspectErr
	}
	if s.leaseViews != nil {
		return s.leaseViews[raw], nil
	}
	return coopclient.LeaseView{}, nil
}

func (s *stubBackend) Holder() string { return s.holder }

func (s *stubBackend) Route(domain, name string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routeCalls++
	if s.routeErrFn != nil {
		if err := s.routeErrFn(name); err != nil {
			return "", "", err
		}
	}
	if s.routeErr != nil {
		return "", "", s.routeErr
	}
	return s.routeKind, s.routeKey, nil
}

func (s *stubBackend) Acquire(leaseKey string, ttl time.Duration) (bool, string, int64, error) {
	if s.acquireErr != nil {
		return false, "", 0, s.acquireErr
	}
	if s.acquireByKey != nil {
		if s.acquireByKey[leaseKey] {
			return true, "", 7, nil
		}
		return false, s.acquireHolder, 7, nil
	}
	return s.acquireGranted, s.acquireHolder, 7, nil
}

func (s *stubBackend) Renew(leaseKey string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	s.renewCalls++
	s.mu.Unlock()
	return s.renewOK, s.renewErr
}

func (s *stubBackend) Release(leaseKey string) (bool, error) {
	s.mu.Lock()
	s.releaseCalls++
	s.mu.Unlock()
	return s.releaseOK, s.releaseErr
}

func (s *stubBackend) ListHolders() ([]coopclient.Holder, error) {
	return s.holders, s.holdersErr
}

func (s *stubBackend) Integrations() ([]coopclient.Integration, error) {
	return s.ints, s.intsErr
}

func (s *stubBackend) Close() error {
	s.mu.Lock()
	s.closeCalls++
	s.mu.Unlock()
	return nil
}

// newTestServer builds a server wired to a stub backend with a fast renew TTL.
func newTestServer(t *testing.T, be backend) *server {
	t.Helper()
	return &server{
		version: "test-1.2.3",
		cfg:     coopclient.Config{LeaseTTL: 2 * time.Second, Session: "sess-1"},
		dial:    func(coopclient.Config) (backend, error) { return be, nil },
		logf:    func(string, ...any) {},
		be:      be,
		claimed: map[string]string{},
	}
}

// callTextResult invokes a tool and returns its single text block + isError.
func callTextResult(t *testing.T, s *server, name, path string) (string, bool) {
	t.Helper()
	var p toolsCallParams
	p.Name = name
	p.Arguments.Path = path
	res := s.callTool(p)
	if len(res.Content) != 1 {
		t.Fatalf("expected exactly 1 content block, got %d", len(res.Content))
	}
	if res.Content[0].Type != "text" {
		t.Fatalf("expected text content, got %q", res.Content[0].Type)
	}
	return res.Content[0].Text, res.IsError
}

func TestToolLocksEmpty(t *testing.T) {
	s := newTestServer(t, &stubBackend{holder: "me"})
	text, isErr := callTextResult(t, s, "mad_locks", "")
	if isErr {
		t.Fatalf("locks should not be an error")
	}
	if text != "No leases are currently held." {
		t.Fatalf("unexpected text: %q", text)
	}
}

func TestToolLocksWithYou(t *testing.T) {
	be := &stubBackend{
		holder: "me",
		holders: []coopclient.Holder{
			{Key: "k1", Holder: "me", Fence: 3},
			{Key: "k2", Holder: "other", Fence: 9},
		},
	}
	s := newTestServer(t, be)
	text, isErr := callTextResult(t, s, "mad_locks", "")
	if isErr {
		t.Fatalf("locks should not be an error")
	}
	if !strings.Contains(text, "• me (you) holds k1 (fence 3)") {
		t.Fatalf("missing self line: %q", text)
	}
	if !strings.Contains(text, "• other holds k2 (fence 9)") {
		t.Fatalf("missing other line: %q", text)
	}
	if strings.Contains(text, "other (you)") {
		t.Fatalf("should not mark other as you: %q", text)
	}
}

func TestToolLocksRendersPaths(t *testing.T) {
	// Daemon emits keys base64-encoded; locks decodes then classifies them.
	pathKey := base64.StdEncoding.EncodeToString([]byte("mad-substrate:convergent:v1:tokens.ts"))
	trunkKey := base64.StdEncoding.EncodeToString([]byte(manifest.TrunkKey))
	opaque := base64.StdEncoding.EncodeToString([]byte("some-other-key"))
	be := &stubBackend{
		holder: "me",
		holders: []coopclient.Holder{
			{Key: pathKey, Holder: "me", Fence: 1},
			{Key: trunkKey, Holder: "other", Fence: 2},
			{Key: opaque, Holder: "third", Fence: 3},
		},
	}
	s := newTestServer(t, be)
	text, isErr := callTextResult(t, s, "mad_locks", "")
	if isErr {
		t.Fatalf("locks should not be an error: %q", text)
	}
	if !strings.Contains(text, `• me (you) holds convergent path "tokens.ts" (fence 1)`) {
		t.Fatalf("per-path key must render the file: %q", text)
	}
	if !strings.Contains(text, "• other holds the trunk (fence 2)") {
		t.Fatalf("trunk key must render as the trunk: %q", text)
	}
	if !strings.Contains(text, "• third holds "+opaque+" (fence 3)") {
		t.Fatalf("unknown key must fall back to the opaque base64 key: %q", text)
	}
}

func TestToolClassifyForkable(t *testing.T) {
	be := &stubBackend{routeKind: "forkable", routeKey: ""}
	s := newTestServer(t, be)
	text, isErr := callTextResult(t, s, "mad_classify", "src/foo.go")
	if isErr {
		t.Fatalf("classify should not be an error")
	}
	want := `"src/foo.go" -> forkable — no coordination needed.`
	if text != want {
		t.Fatalf("got %q want %q", text, want)
	}
}

func TestToolClassifyConvergent(t *testing.T) {
	be := &stubBackend{routeKind: "convergent", routeKey: "lease-abc"}
	s := newTestServer(t, be)
	text, isErr := callTextResult(t, s, "mad_classify", "go.mod")
	if isErr {
		t.Fatalf("classify should not be an error")
	}
	want := `"go.mod" -> convergent — needs coordination (claim before editing).`
	if text != want {
		t.Fatalf("got %q want %q", text, want)
	}
}

func TestToolClassifyMissingPath(t *testing.T) {
	s := newTestServer(t, &stubBackend{})
	text, isErr := callTextResult(t, s, "mad_classify", "  ")
	if !isErr {
		t.Fatalf("missing path should be an error")
	}
	if text != "path is required" {
		t.Fatalf("got %q", text)
	}
}

func TestToolClaimForkable(t *testing.T) {
	be := &stubBackend{routeKind: "forkable", routeKey: ""}
	s := newTestServer(t, be)
	text, isErr := callTextResult(t, s, "mad_claim", "README.md")
	if isErr {
		t.Fatalf("forkable claim should not be an error")
	}
	if !strings.Contains(text, "is forkable — no claim needed") {
		t.Fatalf("got %q", text)
	}
	if len(s.claimed) != 0 {
		t.Fatalf("forkable claim must not track a key")
	}
}

func TestToolClaimGranted(t *testing.T) {
	be := &stubBackend{routeKind: "convergent", routeKey: "lease-1", acquireGranted: true}
	s := newTestServer(t, be)
	text, isErr := callTextResult(t, s, "mad_claim", "go.mod")
	if isErr {
		t.Fatalf("granted claim should not be an error")
	}
	if !strings.Contains(text, `Claimed "go.mod" (convergent).`) {
		t.Fatalf("got %q", text)
	}
	if s.claimed["go.mod"] != "lease-1" {
		t.Fatalf("granted claim must be tracked, got %v", s.claimed)
	}
}

func TestToolClaimDenied(t *testing.T) {
	be := &stubBackend{routeKind: "convergent", routeKey: "lease-1", acquireGranted: false, acquireHolder: "rival"}
	s := newTestServer(t, be)
	text, isErr := callTextResult(t, s, "mad_claim", "go.mod")
	if !isErr {
		t.Fatalf("denied claim must be an error")
	}
	if !strings.Contains(text, "currently held by another session (rival)") {
		t.Fatalf("got %q", text)
	}
	if len(s.claimed) != 0 {
		t.Fatalf("denied claim must not track")
	}
}

func TestToolReleaseForkable(t *testing.T) {
	be := &stubBackend{routeKind: "forkable", routeKey: ""}
	s := newTestServer(t, be)
	text, isErr := callTextResult(t, s, "mad_release", "README.md")
	if isErr {
		t.Fatalf("forkable release should not be an error")
	}
	if !strings.Contains(text, "is forkable — nothing to release") {
		t.Fatalf("got %q", text)
	}
}

func TestToolReleaseOK(t *testing.T) {
	be := &stubBackend{routeKind: "convergent", routeKey: "lease-1", releaseOK: true}
	s := newTestServer(t, be)
	s.track("go.mod", "lease-1")
	text, isErr := callTextResult(t, s, "mad_release", "go.mod")
	if isErr {
		t.Fatalf("release should not be an error")
	}
	if text != `Released "go.mod".` {
		t.Fatalf("got %q", text)
	}
	if len(s.claimed) != 0 {
		t.Fatalf("release must untrack, got %v", s.claimed)
	}
}

func TestToolReleaseNotHeld(t *testing.T) {
	be := &stubBackend{routeKind: "convergent", routeKey: "lease-1", releaseOK: false}
	s := newTestServer(t, be)
	text, isErr := callTextResult(t, s, "mad_release", "go.mod")
	if isErr {
		t.Fatalf("not-held release should not be an error")
	}
	if !strings.Contains(text, "was not held by you; nothing released") {
		t.Fatalf("got %q", text)
	}
}

func TestToolStatus(t *testing.T) {
	be := &stubBackend{
		holder: "me",
		holders: []coopclient.Holder{
			{Key: "kb", Holder: "me"},
			{Key: "ka", Holder: "me"},
			{Key: "kz", Holder: "other"},
		},
		ints: []coopclient.Integration{
			{ID: "i-1", Branch: "nm/a", State: "validating"},
			{ID: "i-2", Branch: "nm/b", State: "merged"},
			{ID: "i-3", Branch: "nm/c", State: "received"},
		},
	}
	s := newTestServer(t, be)
	text, isErr := callTextResult(t, s, "mad_status", "")
	if isErr {
		t.Fatalf("status should not be an error")
	}
	if !strings.Contains(text, "Your identity: me (session sess-1)") {
		t.Fatalf("identity line wrong: %q", text)
	}
	// mine sorted: ka, kb
	if !strings.Contains(text, "You hold: ka, kb") {
		t.Fatalf("hold line wrong: %q", text)
	}
	if !strings.Contains(text, "All lease holders: 3") {
		t.Fatalf("count line wrong: %q", text)
	}
	if !strings.Contains(text, "i-1 (nm/a: validating)") || !strings.Contains(text, "i-3 (nm/c: received)") {
		t.Fatalf("in-flight line wrong: %q", text)
	}
	if strings.Contains(text, "i-2") {
		t.Fatalf("merged integration must not be in-flight: %q", text)
	}
}

func TestToolStatusNothingHeld(t *testing.T) {
	be := &stubBackend{holder: "me"}
	s := newTestServer(t, be)
	text, _ := callTextResult(t, s, "mad_status", "")
	if !strings.Contains(text, "You hold: nothing") {
		t.Fatalf("expected 'nothing': %q", text)
	}
	if !strings.Contains(text, "In-flight integrations: none") {
		t.Fatalf("expected 'none': %q", text)
	}
}

func TestUnknownTool(t *testing.T) {
	s := newTestServer(t, &stubBackend{})
	text, isErr := callTextResult(t, s, "mad_nope", "")
	if !isErr {
		t.Fatalf("unknown tool should be an error")
	}
	if !strings.Contains(text, `unknown tool "mad_nope"`) {
		t.Fatalf("got %q", text)
	}
}

// failSoftBackend returns a transport error from the daemon-facing calls.
func TestFailSoftTransportNote(t *testing.T) {
	be := &stubBackend{holdersErr: transportError("boom")}
	s := newTestServer(t, be)
	// dial returns the SAME failing backend, so the single re-Dial+retry also
	// fails -> fail-soft note.
	text, isErr := callTextResult(t, s, "mad_locks", "")
	if !isErr {
		t.Fatalf("transport failure must be isError")
	}
	if text != failSoftNote {
		t.Fatalf("expected fail-soft note, got %q", text)
	}
}

func TestFailSoftReDialRecovers(t *testing.T) {
	failing := &stubBackend{holdersErr: transportError("boom")}
	healthy := &stubBackend{holder: "me", holders: []coopclient.Holder{{Key: "k", Holder: "me", Fence: 1}}}
	s := newTestServer(t, failing)
	// The first re-Dial swaps in the healthy backend so the retry succeeds.
	s.dial = func(coopclient.Config) (backend, error) { return healthy, nil }
	text, isErr := callTextResult(t, s, "mad_locks", "")
	if isErr {
		t.Fatalf("re-dial should recover, got error: %q", text)
	}
	if !strings.Contains(text, "• me (you) holds k (fence 1)") {
		t.Fatalf("got %q", text)
	}
}

func TestNonTransportVerdictSurfaced(t *testing.T) {
	be := &stubBackend{routeKind: "convergent", routeKey: "k", acquireErr: verdictError("conflict")}
	s := newTestServer(t, be)
	text, isErr := callTextResult(t, s, "mad_claim", "go.mod")
	if !isErr {
		t.Fatalf("a daemon verdict should be isError")
	}
	if text == failSoftNote {
		t.Fatalf("a non-transport verdict must NOT be masked by the fail-soft note")
	}
	if !strings.Contains(text, "conflict") {
		t.Fatalf("verdict message should surface: %q", text)
	}
}

func TestNoBackendFailSoft(t *testing.T) {
	s := newTestServer(t, nil)
	s.be = nil
	// dial fails -> no backend at all.
	s.dial = func(coopclient.Config) (backend, error) { return nil, transportError("down") }
	text, isErr := callTextResult(t, s, "mad_status", "")
	if !isErr || text != failSoftNote {
		t.Fatalf("no-backend must yield fail-soft note, got isErr=%v %q", isErr, text)
	}
}

// ----- transport / protocol level tests via serveWith -----

// runServe feeds input lines through serveWith (builder role) and returns the
// output lines.
func runServe(t *testing.T, be backend, input string) []string {
	return runServeRole(t, be, "builder", input)
}

// runServeRole is runServe with an explicit role so role-gating can be exercised.
func runServeRole(t *testing.T, be backend, role, input string) []string {
	t.Helper()
	// Hermetic runtime dir: a granted integrator now writes a presence pidfile
	// beside the (resolved) ledger. Pin it to a temp dir so the suite never
	// touches the developer's real ~/.mad-substrate.
	t.Setenv("MAD_RUNTIME_DIR", t.TempDir())
	var out bytes.Buffer
	cfg := coopclient.Config{LeaseTTL: 2 * time.Second, Session: "sess-1"}
	dial := func(coopclient.Config) (backend, error) { return be, nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := serveWith(ctx, strings.NewReader(input), &out, "v9", role, func(string, ...any) {}, cfg, dial); err != nil {
		t.Fatalf("serveWith: %v", err)
	}
	var lines []string
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			lines = append(lines, sc.Text())
		}
	}
	return lines
}

func runServeRoleAfterFirstEventPoll(t *testing.T, be *stubBackend, role, input string) []string {
	t.Helper()
	t.Setenv("MAD_RUNTIME_DIR", t.TempDir())
	t.Setenv("MAD_LAUNCHED", "")
	be.eventsNotify = make(chan struct{})
	var out bytes.Buffer
	cfg := coopclient.Config{LeaseTTL: 2 * time.Second, Session: "sess-1"}
	dial := func(coopclient.Config) (backend, error) { return be, nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serveWith(ctx, pr, &out, "v9", role, func(string, ...any) {}, cfg, dial)
	}()
	select {
	case <-be.eventsNotify:
	case <-time.After(500 * time.Millisecond):
		cancel()
		_ = pw.Close()
		t.Fatal("event poll did not start")
	}
	if _, err := pw.Write([]byte(input)); err != nil {
		cancel()
		_ = pw.Close()
		t.Fatalf("write input: %v", err)
	}
	if err := pw.Close(); err != nil {
		cancel()
		t.Fatalf("close input: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveWith: %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("serveWith did not return")
	}
	var lines []string
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			lines = append(lines, sc.Text())
		}
	}
	return lines
}

// ----- integrator singleton enforcement (Inv 13 fail-soft) -----

// TestIntegratorRefusesSecond proves the enforced refusal: a reachable daemon
// that denies the presence lease (another live integrator holds it) makes
// serveWith RETURN the refusal error and serve nothing.
func TestIntegratorRefusesSecond(t *testing.T) {
	be := &stubBackend{acquireGranted: false, acquireHolder: "other"}
	cfg := coopclient.Config{LeaseTTL: 2 * time.Second, Session: "sess-1"}
	dial := func(coopclient.Config) (backend, error) { return be, nil }
	var out bytes.Buffer
	// A request that WOULD produce a response line if we served it, so we can
	// prove the refusal happens before any serve I/O.
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := serveWith(ctx, strings.NewReader(in), &out, "v9", "integrator", func(string, ...any) {}, cfg, dial)
	if err == nil {
		t.Fatalf("a second integrator must be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "already running") || !strings.Contains(err.Error(), "other") {
		t.Fatalf("refusal error must be actionable and name the holder: %v", err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("a refused integrator must not serve any request, got output: %q", out.String())
	}
}

// TestIntegratorGrantedServes is the NON-VACUOUS control for the refusal: with
// the same role and an Acquire that GRANTS, serveWith serves normally and does
// not refuse — proving the refusal is conditional on the deny, not unconditional.
func TestIntegratorGrantedServes(t *testing.T) {
	be := &stubBackend{acquireGranted: true, renewOK: true}
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"
	lines := runServeRole(t, be, "integrator", in)
	if len(lines) != 1 {
		t.Fatalf("a granted integrator must serve, got %v", lines)
	}
}

// TestIntegratorAcquireErrorFailSoft proves Inv 13: a daemon-unreachable / failed
// Acquire RPC must NOT refuse — it degrades to allowing and serves advisory.
func TestIntegratorAcquireErrorFailSoft(t *testing.T) {
	be := &stubBackend{acquireErr: transportError("daemon down"), renewOK: true}
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"
	lines := runServeRole(t, be, "integrator", in)
	if len(lines) != 1 {
		t.Fatalf("an unverifiable singleton (acquire error) must fail-soft and serve, got %v", lines)
	}
}

// TestBuilderNeverRefuses proves builders take no presence lease and are never
// refused regardless of the Acquire result (a deny that would refuse an
// integrator is inert for a builder).
func TestBuilderNeverRefuses(t *testing.T) {
	be := &stubBackend{acquireGranted: false, acquireHolder: "other"}
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"
	lines := runServeRole(t, be, "builder", in)
	if len(lines) != 1 {
		t.Fatalf("a builder must never refuse on a presence deny, got %v", lines)
	}
}

// ----- integrator POOL (R9): opt-in N slots, default singleton -----

// poolDaemon is a stateful fake daemon for the integrator-pool tests: it grants
// the FIRST acquire of each distinct slot key and denies any later acquire of an
// already-held key, modelling N integrators competing for N slots. Holdings
// persist across serveWith calls (the embedded stub's Release is a no-op on the
// held set), so three sequential serveWith runs model three CONCURRENT integrators.
type poolDaemon struct {
	*stubBackend
	pmu     sync.Mutex
	held    map[string]bool
	granted []string // slot keys granted, in acquisition order
}

func newPoolDaemon() *poolDaemon {
	return &poolDaemon{stubBackend: &stubBackend{renewOK: true}, held: map[string]bool{}}
}

func (p *poolDaemon) Acquire(key string, _ time.Duration) (bool, string, int64, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if p.held[key] {
		return false, "busy", 7, nil
	}
	p.held[key] = true
	p.granted = append(p.granted, key)
	return true, "", 7, nil
}

func (p *poolDaemon) grantedKeys() []string {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	out := make([]string, len(p.granted))
	copy(out, p.granted)
	return out
}

// serveRefusedIntegrator runs an integrator serveWith expected to be REFUSED and
// returns its error + any output (which must be empty for a refused integrator).
func serveRefusedIntegrator(t *testing.T, be backend) (error, string) {
	t.Helper()
	var out bytes.Buffer
	cfg := coopclient.Config{LeaseTTL: 2 * time.Second}
	dial := func(coopclient.Config) (backend, error) { return be, nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"
	err := serveWith(ctx, strings.NewReader(in), &out, "v9", "integrator", func(string, ...any) {}, cfg, dial)
	return err, strings.TrimSpace(out.String())
}

// TestIntegratorPoolFillsDistinctSlots proves the opt-in pool (R9): with
// MAD_INTEGRATOR_POOL=2, two integrators BOTH serve on DISTINCT slots
// (slot-0 then slot-1), and a THIRD is REFUSED because all slots are held.
func TestIntegratorPoolFillsDistinctSlots(t *testing.T) {
	t.Setenv("MAD_INTEGRATOR_POOL", "2")
	fake := newPoolDaemon()
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"

	if lines := runServeRole(t, fake, "integrator", in); len(lines) != 1 {
		t.Fatalf("first integrator must serve (slot-0), got %v", lines)
	}
	if lines := runServeRole(t, fake, "integrator", in); len(lines) != 1 {
		t.Fatalf("second integrator must serve on the next free slot (slot-1), got %v", lines)
	}

	err, out := serveRefusedIntegrator(t, fake)
	if err == nil {
		t.Fatalf("a third integrator must be refused when all 2 slots are held")
	}
	if !strings.Contains(err.Error(), "all 2 integrator slots are in use") {
		t.Fatalf("pool-full refusal must name slot exhaustion: %v", err)
	}
	if out != "" {
		t.Fatalf("a refused integrator must not serve any request, got %q", out)
	}

	// The two served integrators must have taken DISTINCT slots, in order.
	want := integratorSlotKeys(2)
	got := fake.grantedKeys()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("pool must fill slot-0 then slot-1; granted %v want %v", got, want)
	}
}

// TestIntegratorPoolDefaultSingleton is the NON-VACUOUS control for the pool:
// with NO MAD_INTEGRATOR_POOL (N=1) the behavior is byte-identical to the
// historic singleton — the FIRST integrator serves on the well-known
// mad-substrate:integrator:v1 key and the SECOND is refused.
func TestIntegratorPoolDefaultSingleton(t *testing.T) {
	// Defensively force unset (empty ⇒ N=1) so ambient env can't perturb the control.
	t.Setenv("MAD_INTEGRATOR_POOL", "")
	fake := newPoolDaemon()
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"

	if lines := runServeRole(t, fake, "integrator", in); len(lines) != 1 {
		t.Fatalf("first integrator must serve, got %v", lines)
	}
	err, out := serveRefusedIntegrator(t, fake)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("default (N=1) must refuse the second integrator with the singleton message, got %v", err)
	}
	if out != "" {
		t.Fatalf("a refused integrator must not serve, got %q", out)
	}
	// The slot used must be the singleton key, never a slot-N key.
	if got := fake.grantedKeys(); len(got) != 1 || got[0] != integratorPresenceKey {
		t.Fatalf("default must use the singleton key; granted %v", got)
	}
}

func TestInitializeHandshake(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n"
	lines := runServe(t, &stubBackend{holder: "me"}, in)
	if len(lines) != 1 {
		t.Fatalf("expected 1 response line, got %d: %v", len(lines), lines)
	}
	var resp struct {
		ID     int `json:"id"`
		Result struct {
			ProtocolVersion string            `json:"protocolVersion"`
			Capabilities    map[string]any    `json:"capabilities"`
			ServerInfo      map[string]string `json:"serverInfo"`
			Instructions    string            `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ID != 1 {
		t.Fatalf("id not echoed: %d", resp.ID)
	}
	if resp.Result.ProtocolVersion != "2025-06-18" {
		t.Fatalf("protocolVersion not echoed: %q", resp.Result.ProtocolVersion)
	}
	if _, ok := resp.Result.Capabilities["tools"]; !ok {
		t.Fatalf("capabilities.tools missing: %v", resp.Result.Capabilities)
	}
	if resp.Result.ServerInfo["name"] != "mad-substrate" || resp.Result.ServerInfo["version"] != "v9" {
		t.Fatalf("serverInfo wrong: %v", resp.Result.ServerInfo)
	}
	if strings.TrimSpace(resp.Result.Instructions) == "" {
		t.Fatalf("instructions must be non-empty")
	}
	if !strings.Contains(resp.Result.Instructions, "governed boundary") {
		t.Fatalf("instructions should carry standing guidance: %q", resp.Result.Instructions)
	}
	// The guidance must NOT steer agents to the removed request_merge tool;
	// convergence is handled outside the session (`mad-substrate integrate` / lead).
	if strings.Contains(resp.Result.Instructions, "request_merge") {
		t.Fatalf("instructions must not reference request_merge: %q", resp.Result.Instructions)
	}
}

func TestInitializeInstructionsRoleCorrect(t *testing.T) {
	builder := initializeInstructions(t, "builder")
	if !strings.Contains(builder, "mad_request_integration") {
		t.Fatalf("builder guidance must tell builders to request integration: %q", builder)
	}
	if !strings.Contains(builder, "[mad-substrate] nudge") || !strings.Contains(builder, "mad_integration_status") {
		t.Fatalf("builder guidance must explain nudge/status feedback loop: %q", builder)
	}
	if strings.Contains(builder, "trunk-side reviewer") {
		t.Fatalf("builder guidance must not use integrator role text: %q", builder)
	}

	defaulted := initializeInstructions(t, "not-a-real-role")
	if defaulted != builder {
		t.Fatalf("unknown role must default to builder guidance\nbuilder: %q\ndefault: %q", builder, defaulted)
	}

	integrator := initializeInstructions(t, "integrator")
	for _, want := range []string{
		"trunk-side reviewer",
		"ALWAYS drain mad_integration_pending",
		"claim",
		"mad_integration_approve",
		"mad_integration_reject",
		"MAD_INTEGRATOR_GATE",
		"[mad-substrate]",
	} {
		if !strings.Contains(integrator, want) {
			t.Fatalf("integrator guidance missing %q: %q", want, integrator)
		}
	}
	if strings.Contains(integrator, "once your work is committed") {
		t.Fatalf("integrator guidance must not tell the reviewer to request integration: %q", integrator)
	}
}

func initializeInstructions(t *testing.T, role string) string {
	t.Helper()
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n"
	be := &stubBackend{acquireGranted: true, renewOK: true}
	lines := runServeRole(t, be, role, in)
	if len(lines) != 1 {
		t.Fatalf("expected 1 response line, got %d: %v", len(lines), lines)
	}
	var resp struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp.Result.Instructions
}

func TestInitializeDefaultProtocolVersion(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	lines := runServe(t, &stubBackend{}, in)
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Result.ProtocolVersion != defaultProtocolVersion {
		t.Fatalf("expected default version, got %q", resp.Result.ProtocolVersion)
	}
}

func TestNotificationsInitializedNoOutput(t *testing.T) {
	in := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	lines := runServe(t, &stubBackend{}, in)
	if len(lines) != 0 {
		t.Fatalf("notification must produce no output, got: %v", lines)
	}
}

func TestPing(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":42,"method":"ping"}` + "\n"
	lines := runServe(t, &stubBackend{}, in)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %v", lines)
	}
	var resp rpcResponse
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(resp.ID) != "42" {
		t.Fatalf("id not echoed: %s", resp.ID)
	}
	if resp.Error != nil {
		t.Fatalf("ping must not error: %v", resp.Error)
	}
}

func TestToolsListBuilderTools(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	lines := runServe(t, &stubBackend{}, in)
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{
		"mad_locks", "mad_classify", "mad_claim",
		"mad_release", "mad_status",
		"mad_request_integration", "mad_integration_status",
	}
	// The dead-end mad_request_merge tool must no longer be advertised:
	// in the worktree grain its branch was never published to the separate
	// trunk.git, so submitting it could only dead-end. Convergence is handled
	// out-of-session by `mad-substrate integrate` / the lead.
	for _, tool := range resp.Result.Tools {
		if tool.Name == "mad_request_merge" {
			t.Fatalf("mad_request_merge must no longer be advertised")
		}
	}
	if len(resp.Result.Tools) != len(want) {
		t.Fatalf("expected %d tools, got %d", len(want), len(resp.Result.Tools))
	}
	for i, tool := range resp.Result.Tools {
		if tool.Name != want[i] {
			t.Fatalf("tool %d: got %q want %q", i, tool.Name, want[i])
		}
		if tool.Description == "" {
			t.Fatalf("tool %q missing description", tool.Name)
		}
		// every inputSchema must be a valid JSON object with additionalProperties:false
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("tool %q schema not JSON: %v", tool.Name, err)
		}
		if schema["type"] != "object" {
			t.Fatalf("tool %q schema type not object", tool.Name)
		}
		if schema["additionalProperties"] != false {
			t.Fatalf("tool %q schema must set additionalProperties:false", tool.Name)
		}
	}
}

func TestToolsCallViaTransport(t *testing.T) {
	be := &stubBackend{routeKind: "forkable", routeKey: ""}
	in := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"mad_classify","arguments":{"path":"x.go"}}}` + "\n"
	lines := runServe(t, be, in)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %v", lines)
	}
	var resp struct {
		ID     int `json:"id"`
		Result struct {
			Content []toolContent `json:"content"`
			IsError bool          `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("tools/call must not be a JSON-RPC error: %v", resp.Error)
	}
	if resp.ID != 5 {
		t.Fatalf("id not echoed: %d", resp.ID)
	}
	if len(resp.Result.Content) != 1 || resp.Result.Content[0].Type != "text" {
		t.Fatalf("bad content: %v", resp.Result.Content)
	}
	if !strings.Contains(resp.Result.Content[0].Text, "forkable") {
		t.Fatalf("got %q", resp.Result.Content[0].Text)
	}
}

func TestPiggybackAppendsQueuedEventNudge(t *testing.T) {
	be := &stubBackend{
		acquireGranted: true,
		renewOK:        true,
		events: []coopclient.IntegrationEvent{
			{Kind: "integration.requested", Branch: "nm/a"},
			{Kind: "integration.requested", Branch: "nm/b"},
		},
	}
	in := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"mad_integration_pending","arguments":{}}}` + "\n"
	lines := runServeRoleAfterFirstEventPoll(t, be, "integrator", in)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %v", lines)
	}
	text := toolTextFromLine(t, lines[0])
	want := "No integration requests are pending.\n[mad-substrate] 2 integration request(s) awaiting review — run mad_integration_pending and process them."
	if text != want {
		t.Fatalf("piggyback text mismatch\ngot:  %q\nwant: %q", text, want)
	}
	if be.eventsBranch != "" {
		t.Fatalf("integrator event polling must use empty branch, got %q", be.eventsBranch)
	}
}

func TestPiggybackEmptyInboxLeavesToolResultByteIdentical(t *testing.T) {
	be := &stubBackend{acquireGranted: true, renewOK: true}
	in := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"mad_integration_pending","arguments":{}}}` + "\n"
	lines := runServeRoleAfterFirstEventPoll(t, be, "integrator", in)
	if got := toolTextFromLine(t, lines[0]); got != "No integration requests are pending." {
		t.Fatalf("empty inbox must not alter result text, got %q", got)
	}
}

func TestPiggybackDisabledWhenLauncherRanSession(t *testing.T) {
	t.Setenv("MAD_LAUNCHED", "1")
	be := &stubBackend{
		acquireGranted: true,
		renewOK:        true,
		events:         []coopclient.IntegrationEvent{{Kind: "integration.requested", Branch: "nm/a"}},
	}
	in := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"mad_integration_pending","arguments":{}}}` + "\n"
	lines := runServeRole(t, be, "integrator", in)
	if got := toolTextFromLine(t, lines[0]); got != "No integration requests are pending." {
		t.Fatalf("launcher-run sessions must not piggyback nudges, got %q", got)
	}
	if be.eventsCalls != 0 {
		t.Fatalf("MAD_LAUNCHED=1 must not start event polling, got %d calls", be.eventsCalls)
	}
}

func TestPiggybackEventErrorsFailSoft(t *testing.T) {
	be := &stubBackend{
		acquireGranted: true,
		renewOK:        true,
		eventsErr:      errors.New("events table unavailable"),
	}
	in := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"mad_integration_pending","arguments":{}}}` + "\n"
	lines := runServeRoleAfterFirstEventPoll(t, be, "integrator", in)
	if got := toolTextFromLine(t, lines[0]); got != "No integration requests are pending." {
		t.Fatalf("event errors must not alter or break tool result, got %q", got)
	}
}

func TestRenderIntegrationNudgeTemplatesAreFixed(t *testing.T) {
	cases := []struct {
		kind   string
		branch string
		count  int
		want   string
	}{
		{
			kind:  "integration.requested",
			count: 3,
			want:  "[mad-substrate] 3 integration request(s) awaiting review — run mad_integration_pending and process them.",
		},
		{
			kind:   "integration.verdict",
			branch: "nm/branch",
			count:  1,
			want:   "[mad-substrate] your integration request on nm/branch has a verdict — run mad_integration_status.",
		},
		{
			kind:   "integration.claimed",
			branch: "nm/branch",
			count:  1,
			want:   "[mad-substrate] your integration request on nm/branch was claimed for review.",
		},
	}
	for _, tc := range cases {
		got, ok := renderIntegrationNudge(tc.kind, tc.branch, tc.count)
		if !ok || got != tc.want {
			t.Fatalf("renderIntegrationNudge(%q, %q, %d) = %q, %v; want %q, true", tc.kind, tc.branch, tc.count, got, ok, tc.want)
		}
	}
	if got, ok := renderIntegrationNudge("integration.unknown", "nm/branch", 1); ok || got != "" {
		t.Fatalf("unknown event kind must render no nudge, got %q ok=%v", got, ok)
	}
}

func toolTextFromLine(t *testing.T, line string) string {
	t.Helper()
	var resp struct {
		Result struct {
			Content []toolContent `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Result.Content) != 1 || resp.Result.Content[0].Type != "text" {
		t.Fatalf("bad tool content: %+v", resp.Result.Content)
	}
	return resp.Result.Content[0].Text
}

func TestUnknownMethodError(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":7,"method":"frobnicate"}` + "\n"
	lines := runServe(t, &stubBackend{}, in)
	var resp rpcResponse
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != codeMethodNotFound {
		t.Fatalf("expected -32601, got %+v", resp.Error)
	}
	if string(resp.ID) != "7" {
		t.Fatalf("id not echoed: %s", resp.ID)
	}
}

func TestUnparseableLineWithIDGetsParseError(t *testing.T) {
	// Valid JSON object with an id but a non-string method: unmarshal of the
	// whole rpcRequest fails (method is typed string), yet salvageID — which
	// reads only the id field — recovers id 3 so we can answer with -32700.
	in := `{"id":3,"method":12345,"jsonrpc":"2.0"}` + "\n"
	lines := runServe(t, &stubBackend{}, in)
	if len(lines) != 1 {
		t.Fatalf("expected 1 parse-error line, got %v", lines)
	}
	var resp rpcResponse
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != codeParseError {
		t.Fatalf("expected -32700, got %+v", resp.Error)
	}
	if string(resp.ID) != "3" {
		t.Fatalf("salvaged id wrong: %s", resp.ID)
	}
}

func TestUnparseableLineNoIDDropped(t *testing.T) {
	in := "this is not json at all\n"
	lines := runServe(t, &stubBackend{}, in)
	if len(lines) != 0 {
		t.Fatalf("unparseable line with no id must be dropped, got: %v", lines)
	}
}

func TestBlankLinesSkipped(t *testing.T) {
	in := "\n   \n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n\n"
	lines := runServe(t, &stubBackend{}, in)
	if len(lines) != 1 {
		t.Fatalf("blank lines should be skipped; expected 1 response, got %v", lines)
	}
}

func TestMultipleMessagesInOrder(t *testing.T) {
	be := &stubBackend{holder: "me"}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n") + "\n"
	lines := runServe(t, be, in)
	// ping -> 1 response, notification -> 0, tools/list -> 1 response = 2 lines
	if len(lines) != 2 {
		t.Fatalf("expected 2 responses, got %d: %v", len(lines), lines)
	}
	var first rpcResponse
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(first.ID) != "1" {
		t.Fatalf("first response should be ping id 1, got %s", first.ID)
	}
}

func TestShutdownReleasesClaimsAndCloses(t *testing.T) {
	be := &stubBackend{releaseOK: true}
	s := &server{
		version: "v",
		cfg:     coopclient.Config{LeaseTTL: time.Second},
		dial:    func(coopclient.Config) (backend, error) { return be, nil },
		logf:    func(string, ...any) {},
		be:      be,
		claimed: map[string]string{"a": "ka", "b": "kb"},
	}
	s.shutdown()
	be.mu.Lock()
	defer be.mu.Unlock()
	if be.releaseCalls != 2 {
		t.Fatalf("expected 2 releases on shutdown, got %d", be.releaseCalls)
	}
	if be.closeCalls != 1 {
		t.Fatalf("expected 1 close, got %d", be.closeCalls)
	}
}

func TestRenewDropsLapsedClaim(t *testing.T) {
	be := &stubBackend{renewOK: false} // renew always reports not-ok
	s := newTestServer(t, be)
	s.track("go.mod", "lease-1")
	s.renewAll()
	if len(s.claimed) != 0 {
		t.Fatalf("a not-ok renew must drop the claim, got %v", s.claimed)
	}
}

func TestRenewKeepsLiveClaim(t *testing.T) {
	be := &stubBackend{renewOK: true}
	s := newTestServer(t, be)
	s.track("go.mod", "lease-1")
	s.renewAll()
	if s.claimed["go.mod"] != "lease-1" {
		t.Fatalf("a successful renew must keep the claim, got %v", s.claimed)
	}
}

func TestServeStopsOnContextCancel(t *testing.T) {
	be := &stubBackend{}
	cfg := coopclient.Config{LeaseTTL: time.Second, Session: "sess-1"}
	dial := func(coopclient.Config) (backend, error) { return be, nil }
	ctx, cancel := context.WithCancel(context.Background())
	// A reader that blocks forever so only ctx-cancel can end serveWith.
	pr, pw := newBlockingReader()
	defer pw.Close()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- serveWith(ctx, pr, &out, "v", "builder", func(string, ...any) {}, cfg, dial)
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveWith returned error on cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveWith did not return after context cancel")
	}
}

// blockingReader is an io.Reader whose Read blocks until the writer end is
// closed, then returns EOF. It lets a test prove serveWith returns on
// ctx-cancel even while a read is outstanding.
type blockingReader struct{ ch chan struct{} }

func newBlockingReader() (*blockingReader, *blockingWriter) {
	ch := make(chan struct{})
	return &blockingReader{ch: ch}, &blockingWriter{ch: ch}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	<-r.ch
	return 0, io.EOF
}

type blockingWriter struct {
	once sync.Once
	ch   chan struct{}
}

func (w *blockingWriter) Close() error {
	w.once.Do(func() { close(w.ch) })
	return nil
}
