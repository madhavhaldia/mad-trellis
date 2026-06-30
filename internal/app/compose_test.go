package app

// Integration test: a real lease/classify round-trip over the Unix socket
// against the composed, frozen daemon. Proves the frozen external contract
// surface works end to end (the handoff readiness gate).

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/manifest"
	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

func TestFrozenSurfaceRoundTrip(t *testing.T) {
	sock := shortSock(t)
	d, closeLedger, err := Build(Config{
		SocketPath: sock,
		LedgerPath: filepath.Join(t.TempDir(), "ledger.db"),
		RepoRoot:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer closeLedger()
	if err := d.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { _ = d.Serve() }()
	defer d.Close()

	a := dialc(t, sock)
	defer a.close()
	key := base64.StdEncoding.EncodeToString(manifest.TrunkKey)

	// acquire over the socket
	var ar struct {
		Granted bool   `json:"granted"`
		Holder  string `json:"holder"`
	}
	resultOf(t, a.call(t, "lease.acquire", map[string]any{"key": key, "ttl_ms": 60000}), &ar)
	if !ar.Granted {
		t.Fatal("first acquire over the socket must grant")
	}

	// Inv 4: the holder is bound to the session — a DIFFERENT connection cannot
	// release another session's lease.
	b := dialc(t, sock)
	defer b.close()
	var brel struct {
		OK bool `json:"ok"`
	}
	resultOf(t, b.call(t, "lease.release", map[string]any{"key": key}), &brel)
	if brel.OK {
		t.Fatal("a different session must NOT release another's lease (Inv 4 holder-binding)")
	}

	// the holder releases its own lease
	var arel struct {
		OK bool `json:"ok"`
	}
	resultOf(t, a.call(t, "lease.release", map[string]any{"key": key}), &arel)
	if !arel.OK {
		t.Fatal("the holder must release its own lease")
	}

	// classify.route(trunk) → convergent + the trunk lease key
	var route struct {
		Kind     string `json:"kind"`
		LeaseKey string `json:"lease_key"`
	}
	resultOf(t, a.call(t, "classify.route", map[string]any{"domain": "trunk"}), &route)
	if route.Kind != "convergent" || route.LeaseKey != key {
		t.Fatalf("trunk must route convergent + the trunk key; got %+v", route)
	}

	// classify.classify(external, undeclared) → singular (default-deny over the wire)
	var cl struct {
		Kind string `json:"kind"`
	}
	resultOf(t, a.call(t, "classify.classify", map[string]any{"domain": "external", "name": "some-saas"}), &cl)
	if cl.Kind != "singular" {
		t.Fatalf("an undeclared external resource must classify singular; got %q", cl.Kind)
	}

	// audit.append is served and routes to the durable sink (no error).
	var ok struct {
		OK bool `json:"ok"`
	}
	resultOf(t, a.call(t, "audit.append", map[string]any{"decision_project": "test", "decision_kind": "smoke"}), &ok)
	if !ok.OK {
		t.Fatal("audit.append over the socket must succeed")
	}
}

// --- minimal JSON-RPC test client -------------------------------------------

type client struct {
	conn net.Conn
	dec  *json.Decoder
	enc  *json.Encoder
	id   int
}

func dialc(t *testing.T, path string) *client {
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
	return &client{conn: conn, dec: json.NewDecoder(conn), enc: json.NewEncoder(conn)}
}

func (c *client) close() { _ = c.conn.Close() }

func (c *client) call(t *testing.T, method string, params any) protocol.Response {
	t.Helper()
	c.id++
	id := json.RawMessage(strconv.Itoa(c.id))
	req := protocol.Request{JSONRPC: protocol.JSONRPCVersion, V: protocol.ContractVersion, ID: &id, Method: method}
	if params != nil {
		b, _ := json.Marshal(params)
		req.Params = b
	}
	if err := c.enc.Encode(&req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp protocol.Response
	if err := c.dec.Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func resultOf(t *testing.T, resp protocol.Response, v any) {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}
	if err := json.Unmarshal(resp.Result, v); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
}

func shortSock(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "nm-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	n := f.Name()
	_ = f.Close()
	_ = os.Remove(n)
	t.Cleanup(func() { _ = os.Remove(n); _ = os.Remove(n + ".lock") })
	return n
}
