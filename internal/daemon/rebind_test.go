package daemon

// Inv 4 exception, surgically guarded: cc.RebindSession is the SINGLE sanctioned
// mutation of a connection's identity after Authenticate. This test proves the
// rebind is visible to a subsequent handler call on the SAME connection (the
// shared-identity property session.attach relies on), and that default
// per-connection minting is otherwise untouched.

import (
	"encoding/json"
	"testing"

	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

func TestRebindSessionChangesIdentitySeenByLaterCall(t *testing.T) {
	d := New(Options{SocketPath: tmpSock(t)})

	// A handler that reports whatever identity the call context currently carries
	// (mirrors session.whoami: identity is connection-bound, read from cc).
	must(d.Registry().Register("test.whoami", func(cc *CallContext, _ json.RawMessage) (json.RawMessage, *protocol.Error) {
		return mustJSON(map[string]string{"session": string(cc.Session)}), nil
	}))

	// One connection's context, as handleConn would build it after Authenticate.
	cc := &CallContext{Session: SessionID("s-1-original"), Daemon: d}

	r1 := d.dispatch(cc, &protocol.Request{
		JSONRPC: protocol.JSONRPCVersion, V: protocol.ContractVersion, Method: "test.whoami",
	})
	if got := sessionOf(t, r1); got != "s-1-original" {
		t.Fatalf("before rebind: want s-1-original, got %q", got)
	}

	// The sanctioned mutation.
	cc.RebindSession(SessionID("s-2-shared"))

	r2 := d.dispatch(cc, &protocol.Request{
		JSONRPC: protocol.JSONRPCVersion, V: protocol.ContractVersion, Method: "test.whoami",
	})
	if got := sessionOf(t, r2); got != "s-2-shared" {
		t.Fatalf("after rebind: a later call on the same connection must see the rebound id; got %q", got)
	}
}

func sessionOf(t *testing.T, resp *protocol.Response) string {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var out struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out.Session
}
