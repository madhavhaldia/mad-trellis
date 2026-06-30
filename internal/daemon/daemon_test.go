package daemon

// Hand-authored invariant tests for project 1 (docs/0004 card 1). These are the
// contract: review-gated, negatives carry positive controls. Not vibe-coded.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

// --- Inv 5-arbiter: the daemon is a PROCESS singleton -----------------------

func TestProcessSingletonRace(t *testing.T) {
	path := tmpSock(t)
	const n = 16
	var wg sync.WaitGroup
	handles := make([]*socketHandle, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			handles[i], errs[i] = acquireSocket(path)
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			winners++
			_ = handles[i].ln.Close()
			releaseLock(handles[i].lockFile)
			continue
		}
		if !errors.Is(errs[i], ErrAlreadyRunning) {
			t.Errorf("loser %d: want ErrAlreadyRunning, got %v", i, errs[i])
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one daemon must bind the socket; got %d winners", winners)
	}
}

func TestStaleSocketReclaim(t *testing.T) {
	path := tmpSock(t)
	// Manufacture a STALE socket: bind, keep the file on close, drop the listener.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close() // file remains, nobody listening => provably dead
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected a stale socket file at %s: %v", path, err)
	}
	got, err := acquireSocket(path)
	if err != nil {
		t.Fatalf("a stale socket must be reclaimed, got error: %v", err)
	}
	_ = got.ln.Close()
	releaseLock(got.lockFile)
}

func TestLiveSocketNoFalseTakeover(t *testing.T) {
	_, path := startDaemon(t, nil) // a LIVE daemon owns the socket
	if _, err := acquireSocket(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("must NOT take over a live holder (no two arbiters); got %v", err)
	}
}

// --- Inv 4: unspoofable, connection-bound identity --------------------------

func TestSocketAuthzNoSpoofing(t *testing.T) {
	_, path := startDaemon(t, nil)
	c := dialClient(t, path)
	defer c.close()
	// Forge an identity in the payload; the daemon must ignore it.
	var got struct {
		Session string `json:"session"`
	}
	mustResult(t, c.call(t, "session.whoami", map[string]string{"session": "FORGED-IDENTITY"}), &got)
	if got.Session == "FORGED-IDENTITY" || got.Session == "" {
		t.Fatalf("identity must be daemon-minted, not payload-forged; got %q", got.Session)
	}
	if !strings.HasPrefix(got.Session, "s-") {
		t.Fatalf("unexpected minted identity shape: %q", got.Session)
	}
}

func TestDistinctIdentitiesPerConnection(t *testing.T) {
	_, path := startDaemon(t, nil)
	whoami := func() string {
		c := dialClient(t, path)
		defer c.close()
		var g struct {
			Session string `json:"session"`
		}
		mustResult(t, c.call(t, "session.whoami", nil), &g)
		return g.Session
	}
	a, b := whoami(), whoami()
	if a == "" || a == b {
		t.Fatalf("N connections must mint N distinct identities; got %q and %q", a, b)
	}
}

// --- Contract freeze: version, taxonomy, not-implemented stub ---------------

func TestContractFreezeStubAndVersion(t *testing.T) {
	d, path := startDaemon(t, nil)
	if err := d.Registry().RegisterStub("future.method"); err != nil {
		t.Fatal(err)
	}
	c := dialClient(t, path)
	defer c.close()
	resp := c.call(t, "future.method", nil)
	if resp.Error == nil || resp.Error.Code != protocol.CodeNotImplemented {
		t.Fatalf("a registered stub must return not-implemented; got %+v", resp.Error)
	}
	if resp.JSONRPC != protocol.JSONRPCVersion || resp.V != protocol.ContractVersion {
		t.Fatalf("every envelope must carry the version (jsonrpc=%q v=%d)", resp.JSONRPC, resp.V)
	}
}

func TestMethodNotFound(t *testing.T) {
	_, path := startDaemon(t, nil)
	c := dialClient(t, path)
	defer c.close()
	if resp := c.call(t, "nope.nope", nil); resp.Error == nil || resp.Error.Code != protocol.CodeMethodNotFound {
		t.Fatalf("want method-not-found; got %+v", resp.Error)
	}
}

func TestVersionMismatchRejected(t *testing.T) {
	_, path := startDaemon(t, nil)
	c := dialClient(t, path)
	defer c.close()
	id := json.RawMessage("1")
	resp := c.callRaw(t, protocol.Request{JSONRPC: protocol.JSONRPCVersion, V: 999, ID: &id, Method: "diag.health"})
	if resp.Error == nil || resp.Error.Code != protocol.CodeInvalidRequest {
		t.Fatalf("an unsupported contract version must be rejected; got %+v", resp.Error)
	}
}

// --- Inv 4 + audit: the daemon stamps the connection-bound session ----------

func TestAuditAppendStampsConnectionSession(t *testing.T) {
	spy := &spySink{}
	_, path := startDaemon(t, spy)
	c := dialClient(t, path)
	defer c.close()
	var who struct {
		Session string `json:"session"`
	}
	mustResult(t, c.call(t, "session.whoami", nil), &who)

	before := spy.len()
	// Forge session + timestamp; the daemon must ignore both and stamp its own.
	resp := c.call(t, "audit.append", map[string]any{
		"decision_project": "test",
		"decision_kind":    "unit",
		"session":          "FORGED",
		"timestamp":        "1999-01-01T00:00:00Z",
		"payload":          map[string]string{"k": "v"},
	})
	if resp.Error != nil {
		t.Fatalf("audit.append errored: %+v", resp.Error)
	}
	if spy.len() != before+1 {
		t.Fatalf("expected exactly one appended record")
	}
	rec, _ := spy.last()
	if string(rec.Session) != who.Session {
		t.Fatalf("recorded session must be the connection's minted id %q, got %q", who.Session, rec.Session)
	}
	if string(rec.Session) == "FORGED" {
		t.Fatalf("a forged session was honored (Inv 4 violated)")
	}
	if time.Since(rec.Timestamp) > time.Minute {
		t.Fatalf("daemon must stamp a fresh timestamp, got %v", rec.Timestamp)
	}
	// Missing required fields => client-fault (positive control on validation).
	if bad := c.call(t, "audit.append", map[string]any{"decision_kind": "x"}); bad.Error == nil || bad.Error.Code != protocol.CodeInvalidParams {
		t.Fatalf("want invalid-params for a malformed audit call; got %+v", bad.Error)
	}
}

// --- Inv 10-decoupling: no agent/host dialect in the daemon API -------------

func TestDecouplingNoAgentDialect(t *testing.T) {
	d, _ := startDaemon(t, nil)
	for _, m := range d.Registry().Methods() {
		if hasAgentDialect(m) {
			t.Fatalf("daemon API leaks an agent/host dialect method %q (Inv 10)", m)
		}
	}
	// Positive control: the detector is non-vacuous.
	if !hasAgentDialect("mcp.tools/call") {
		t.Fatal("decoupling detector is vacuous — it would never catch an MCP method")
	}
}

// --- Inv 2(b): dispatch is deterministic ------------------------------------

func TestDeterministicDispatch(t *testing.T) {
	_, path := startDaemon(t, nil)
	c := dialClient(t, path)
	defer c.close()
	r1 := c.call(t, "session.whoami", nil)
	r2 := c.call(t, "session.whoami", nil)
	if string(r1.Result) != string(r2.Result) {
		t.Fatalf("dispatch must be deterministic on one connection: %s vs %s", r1.Result, r2.Result)
	}
}

// --- helpers ----------------------------------------------------------------

func hasAgentDialect(name string) bool {
	n := strings.ToLower(name)
	for _, tok := range []string{"mcp", "claude", "codex", "anthropic", "openai", "hook"} {
		if strings.Contains(n, tok) {
			return true
		}
	}
	return false
}

func tmpSock(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "nm-*.sock") // short path: Unix sun_path is ~104 bytes
	if err != nil {
		t.Fatalf("tmp socket: %v", err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	t.Cleanup(func() { _ = os.Remove(name); _ = os.Remove(name + ".lock") })
	return name
}

func startDaemon(t *testing.T, audit AuditSink) (*Daemon, string) {
	t.Helper()
	path := tmpSock(t)
	opts := Options{SocketPath: path}
	if audit != nil {
		opts.Audit = audit
	}
	d := New(opts)
	if err := d.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	go func() { _ = d.Serve() }()
	t.Cleanup(func() { _ = d.Close() })
	return d, path
}

type spySink struct {
	mu   sync.Mutex
	recs []AuditRecord
}

func (s *spySink) Append(r AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, r)
	return nil
}

func (s *spySink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recs)
}

func (s *spySink) last() (AuditRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.recs) == 0 {
		return AuditRecord{}, false
	}
	return s.recs[len(s.recs)-1], true
}

type testClient struct {
	conn net.Conn
	dec  *json.Decoder
	enc  *json.Encoder
	idc  int
}

func dialClient(t *testing.T, path string) *testClient {
	t.Helper()
	var conn net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if conn, err = net.DialTimeout("unix", path, time.Second); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return &testClient{conn: conn, dec: json.NewDecoder(conn), enc: json.NewEncoder(conn)}
}

func (c *testClient) close() { _ = c.conn.Close() }

func (c *testClient) call(t *testing.T, method string, params any) protocol.Response {
	t.Helper()
	c.idc++
	id := json.RawMessage(fmt.Sprintf("%d", c.idc))
	req := protocol.Request{JSONRPC: protocol.JSONRPCVersion, V: protocol.ContractVersion, ID: &id, Method: method}
	if params != nil {
		req.Params = mustJSON(params)
	}
	return c.callRaw(t, req)
}

func (c *testClient) callRaw(t *testing.T, req protocol.Request) protocol.Response {
	t.Helper()
	if err := c.enc.Encode(&req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp protocol.Response
	if err := c.dec.Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func mustResult(t *testing.T, resp protocol.Response, v any) {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if err := json.Unmarshal(resp.Result, v); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
}
