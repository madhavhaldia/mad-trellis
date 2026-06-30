package coopclient

import (
	"encoding/json"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

// recordedCall is one request the fake daemon saw.
type recordedCall struct {
	Method string
	Params json.RawMessage
}

// fakeDaemon is a minimal unix-socket JSON-RPC server that records every request
// and answers each method with a canned result (defaulting to {}). It always
// answers session.whoami so Dial records a holder. It lets coopclient's new
// integration.* / lease.inspect methods be asserted at the wire level (method
// name + param shape) without a real daemon.
type fakeDaemon struct {
	mu      sync.Mutex
	calls   []recordedCall
	results map[string]string
	ln      net.Listener
	socket  string
}

func newFakeDaemon(t *testing.T, results map[string]string) *fakeDaemon {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "nm-coop-*.sock")
	if err != nil {
		t.Fatalf("tmp socket: %v", err)
	}
	sock := f.Name()
	_ = f.Close()
	_ = os.Remove(sock)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &fakeDaemon{results: results, ln: ln, socket: sock}
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(sock) })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serve(conn)
		}
	}()
	return d
}

func (d *fakeDaemon) serve(conn net.Conn) {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req protocol.Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		d.mu.Lock()
		d.calls = append(d.calls, recordedCall{Method: req.Method, Params: append(json.RawMessage(nil), req.Params...)})
		d.mu.Unlock()

		res := d.results[req.Method]
		if req.Method == "session.whoami" {
			res = `{"session":"holder-x"}`
		}
		if res == "" {
			res = `{}`
		}
		_ = enc.Encode(&protocol.Response{
			JSONRPC: protocol.JSONRPCVersion,
			V:       protocol.ContractVersion,
			ID:      req.ID,
			Result:  json.RawMessage(res),
		})
	}
}

// paramsFor returns the recorded params of the first call to method.
func (d *fakeDaemon) paramsFor(t *testing.T, method string) map[string]any {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.calls {
		if c.Method == method {
			var m map[string]any
			if err := json.Unmarshal(c.Params, &m); err != nil {
				t.Fatalf("params for %s not an object: %v (%s)", method, err, c.Params)
			}
			return m
		}
	}
	t.Fatalf("method %s was never called; saw %v", method, d.methods())
	return nil
}

func (d *fakeDaemon) methods() []string {
	var ms []string
	for _, c := range d.calls {
		ms = append(ms, c.Method)
	}
	return ms
}

func dialFake(t *testing.T, d *fakeDaemon) *Client {
	t.Helper()
	c, err := Dial(Config{Socket: d.socket, RPCTimeout: 2 * time.Second, LeaseTTL: 60 * time.Second})
	if err != nil {
		t.Fatalf("dial fake: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRequestIntegrationWire(t *testing.T) {
	d := newFakeDaemon(t, map[string]string{
		"integration.request": `{"id":"nm/s-1","state":"pending"}`,
	})
	c := dialFake(t, d)

	id, state, err := c.RequestIntegration("nm/s-1", "a title")
	if err != nil {
		t.Fatalf("RequestIntegration: %v", err)
	}
	if id != "nm/s-1" || state != "pending" {
		t.Fatalf("got id=%q state=%q", id, state)
	}
	p := d.paramsFor(t, "integration.request")
	if p["branch"] != "nm/s-1" {
		t.Fatalf("branch param wrong: %v", p)
	}
	if p["title"] != "a title" {
		t.Fatalf("title param wrong: %v", p)
	}
}

func TestRequestIntegrationOmitsEmptyTitle(t *testing.T) {
	d := newFakeDaemon(t, map[string]string{"integration.request": `{"id":"x","state":"pending"}`})
	c := dialFake(t, d)
	if _, _, err := c.RequestIntegration("nm/x", ""); err != nil {
		t.Fatalf("RequestIntegration: %v", err)
	}
	p := d.paramsFor(t, "integration.request")
	if _, ok := p["title"]; ok {
		t.Fatalf("empty title must be omitted from params, got %v", p)
	}
}

func TestIntegrationStatusWire(t *testing.T) {
	d := newFakeDaemon(t, map[string]string{
		"integration.status": `{"found":true,"id":"nm/x","state":"changes_requested","feedback":"fix it","merge":""}`,
	})
	c := dialFake(t, d)
	found, id, state, feedback, merge, err := c.IntegrationStatus("nm/x")
	if err != nil {
		t.Fatalf("IntegrationStatus: %v", err)
	}
	if !found || id != "nm/x" || state != "changes_requested" || feedback != "fix it" || merge != "" {
		t.Fatalf("decoded wrong: found=%v id=%q state=%q fb=%q merge=%q", found, id, state, feedback, merge)
	}
	if p := d.paramsFor(t, "integration.status"); p["branch"] != "nm/x" {
		t.Fatalf("branch param wrong: %v", p)
	}
}

func TestIntegrationPendingWire(t *testing.T) {
	d := newFakeDaemon(t, map[string]string{
		"integration.pending": `{"pending":[{"id":"nm/a","branch":"nm/a","title":"t","state":"pending","created_at_ms":42}]}`,
	})
	c := dialFake(t, d)
	ps, err := c.IntegrationPending()
	if err != nil {
		t.Fatalf("IntegrationPending: %v", err)
	}
	if len(ps) != 1 || ps[0].ID != "nm/a" || ps[0].Title != "t" || ps[0].CreatedAtMs != 42 {
		t.Fatalf("decoded wrong: %+v", ps)
	}
}

func TestIntegrationClaimWire(t *testing.T) {
	d := newFakeDaemon(t, map[string]string{
		"integration.claim": `{"ok":true,"branch":"nm/a","title":"t"}`,
	})
	c := dialFake(t, d)
	ok, branch, title, err := c.IntegrationClaim("nm/a")
	if err != nil {
		t.Fatalf("IntegrationClaim: %v", err)
	}
	if !ok || branch != "nm/a" || title != "t" {
		t.Fatalf("decoded wrong: ok=%v branch=%q title=%q", ok, branch, title)
	}
	if p := d.paramsFor(t, "integration.claim"); p["id"] != "nm/a" {
		t.Fatalf("id param wrong: %v", p)
	}
}

func TestIntegrationVerdictApproveWire(t *testing.T) {
	d := newFakeDaemon(t, map[string]string{"integration.verdict": `{"ok":true,"state":"merged"}`})
	c := dialFake(t, d)
	ok, state, err := c.IntegrationVerdict("nm/a", "approve", "", "deadbeef")
	if err != nil {
		t.Fatalf("IntegrationVerdict: %v", err)
	}
	if !ok || state != "merged" {
		t.Fatalf("decoded wrong: ok=%v state=%q", ok, state)
	}
	p := d.paramsFor(t, "integration.verdict")
	if p["id"] != "nm/a" || p["decision"] != "approve" || p["merge"] != "deadbeef" {
		t.Fatalf("params wrong: %v", p)
	}
	if _, ok := p["feedback"]; ok {
		t.Fatalf("empty feedback must be omitted on approve, got %v", p)
	}
}

func TestIntegrationVerdictRejectWire(t *testing.T) {
	d := newFakeDaemon(t, map[string]string{"integration.verdict": `{"ok":true,"state":"changes_requested"}`})
	c := dialFake(t, d)
	if _, _, err := c.IntegrationVerdict("nm/a", "reject", "needs work", ""); err != nil {
		t.Fatalf("IntegrationVerdict: %v", err)
	}
	p := d.paramsFor(t, "integration.verdict")
	if p["decision"] != "reject" || p["feedback"] != "needs work" {
		t.Fatalf("params wrong: %v", p)
	}
	if _, ok := p["merge"]; ok {
		t.Fatalf("empty merge must be omitted on reject, got %v", p)
	}
}

func TestInspectWire(t *testing.T) {
	d := newFakeDaemon(t, map[string]string{
		"lease.inspect": `{"exists":true,"holder":"integ-1","expires_at_ms":99,"fence":3,"held":true}`,
	})
	c := dialFake(t, d)
	view, err := c.Inspect("a2V5") // base64 of some key bytes
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !view.Exists || view.Holder != "integ-1" || !view.Held || view.Fence != 3 || view.ExpiresAtMs != 99 {
		t.Fatalf("decoded wrong: %+v", view)
	}
	if p := d.paramsFor(t, "lease.inspect"); p["key"] != "a2V5" {
		t.Fatalf("key param wrong: %v", p)
	}
}
