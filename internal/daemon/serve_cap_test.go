package daemon

// FIX 1 (availability): Serve bounds in-flight handleConn goroutines with a
// concurrent-connection semaphore so a connection that connects then never
// speaks cannot park a goroutine+fd indefinitely. These tests drive the cap with
// a deliberately tiny maxConns and prove BOTH halves: the cap actually blocks a
// connection beyond the limit (the clause), AND it recovers once a slot frees
// (the non-vacuous control — proving the cap is a live bound, not a permanent
// wedge).

import (
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/protocol"
)

// startCappedDaemon starts a real daemon whose connection semaphore is sized to
// maxConns, so the cap is exercisable deterministically.
func startCappedDaemon(t *testing.T, maxConns int) string {
	t.Helper()
	path := tmpSock(t)
	d := New(Options{SocketPath: path})
	d.maxConns = maxConns
	if err := d.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	go func() { _ = d.Serve() }()
	t.Cleanup(func() { _ = d.Close() })
	return path
}

// dialRaw opens a raw connection to the daemon socket (no testClient timeouts so
// we can impose our own read deadlines).
func dialRaw(t *testing.T, path string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// whoamiOver issues a session.whoami over conn and decodes the session id. It is
// used to PROVE a connection was accepted and is being handled (thus holding a
// semaphore slot) before we test the cap.
func whoamiOver(t *testing.T, conn net.Conn, id int) string {
	t.Helper()
	rid := json.RawMessage(fmt.Sprintf("%d", id))
	req := protocol.Request{JSONRPC: protocol.JSONRPCVersion, V: protocol.ContractVersion, ID: &rid, Method: "session.whoami"}
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		t.Fatalf("encode whoami: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var resp protocol.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode whoami: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	if resp.Error != nil {
		t.Fatalf("whoami error: %+v", resp.Error)
	}
	var out struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("unmarshal whoami: %v", err)
	}
	return out.Session
}

// TestServeConnCapBlocksAndRecovers proves the concurrent-connection bound: with
// maxConns=2, two live connections occupy both slots, a third connection is NOT
// serviced (it sits in the backlog because Serve will not Accept past the cap),
// and once one of the first two closes the third is serviced — demonstrating the
// cap is a genuine, self-healing bound rather than a permanent lockout.
func TestServeConnCapBlocksAndRecovers(t *testing.T) {
	path := startCappedDaemon(t, 2)

	// Two connections that get accepted, handled, and then sit idle in the Decode
	// loop holding the two available slots.
	c1 := dialRaw(t, path)
	c2 := dialRaw(t, path)
	if s := whoamiOver(t, c1, 1); s == "" {
		t.Fatal("conn1 must be served")
	}
	if s := whoamiOver(t, c2, 2); s == "" {
		t.Fatal("conn2 must be served")
	}

	// Third connection: dial succeeds (kernel backlog) but Serve is blocked on the
	// semaphore before Accept, so no handler runs and no reply ever comes.
	c3 := dialRaw(t, path)
	rid := json.RawMessage("3")
	req := protocol.Request{JSONRPC: protocol.JSONRPCVersion, V: protocol.ContractVersion, ID: &rid, Method: "session.whoami"}
	if err := json.NewEncoder(c3).Encode(&req); err != nil {
		t.Fatalf("encode on c3: %v", err)
	}
	_ = c3.SetReadDeadline(time.Now().Add(600 * time.Millisecond))
	var resp protocol.Response
	if err := json.NewDecoder(c3).Decode(&resp); err == nil {
		t.Fatalf("conn3 must NOT be serviced while the cap is saturated; got a response: %+v", resp)
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("conn3 read should time out (blocked, unaccepted), got: %v", err)
	}

	// Control: free a slot. The daemon must now accept and service conn3, proving
	// the cap recovers and is not a permanent wedge.
	_ = c1.Close()
	if s := whoamiOver(t, c3, 4); s == "" {
		t.Fatal("conn3 must be serviced once a slot frees (cap must recover)")
	}
}
