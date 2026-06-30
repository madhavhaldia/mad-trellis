package watch

// Bounded-client tests: the read-only client MUST never freeze the TUI. A
// daemon that accepts the connection but never replies, or a socket that
// refuses/closes, must yield a timed-out/errored call WITHIN ~the deadline (not
// a hang) — and the model/fetcher must surface "unavailable" rather than crash.

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

// tmpSockPath returns a short, unused Unix socket path that is cleaned up.
func tmpSockPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "nm-watch-*.sock")
	if err != nil {
		t.Fatalf("tmp socket: %v", err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	t.Cleanup(func() { _ = os.Remove(name) })
	return name
}

// silentServer accepts connections but NEVER replies (the wedged-daemon case).
// It holds each conn open so the client's read deadline — not an EOF — is what
// must rescue us. Accepted conns are tracked under a mutex (no channel) so the
// cleanup can close them without racing the accept goroutine.
func silentServer(t *testing.T) string {
	t.Helper()
	path := tmpSockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn) // hold open; never read/write
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		conns = nil
		mu.Unlock()
	})
	return path
}

func TestClientCallTimesOutOnWedgedDaemon(t *testing.T) {
	path := silentServer(t)
	cl, err := Dial(path, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("dial (accept succeeds): %v", err)
	}
	defer cl.Close()

	start := time.Now()
	_, err = cl.Health()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a wedged daemon must yield an ERROR, not a successful call")
	}
	// Must return at ~the deadline, NOT hang. Generous upper bound to avoid flake.
	if elapsed > 3*time.Second {
		t.Fatalf("call did not respect the read deadline (took %v) — TUI would freeze", elapsed)
	}
}

func TestDialRefusedOnDeadSocket(t *testing.T) {
	// A path with no listener: dial must fail promptly (the CLI then prints a
	// friendly message and exits cleanly).
	path := filepath.Join(t.TempDir(), "nonexistent.sock")
	start := time.Now()
	_, err := Dial(path, 200*time.Millisecond)
	if err == nil {
		t.Fatal("dialing a dead socket must error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("dial to a dead socket hung: %v", time.Since(start))
	}
}

func TestFetcherSurfacesUnavailableOnWedgedDaemon(t *testing.T) {
	path := silentServer(t)
	cl, err := Dial(path, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cl.Close()

	fetch := NewClientFetcher(cl, 10)
	start := time.Now()
	snap := fetch()
	elapsed := time.Since(start)

	// Every panel must be unavailable, and the daemon marked unreachable — but the
	// call returns (bounded), it does not hang.
	if snap.DaemonReachable {
		t.Fatal("a wedged daemon must NOT be reported reachable")
	}
	if snap.Leases.Available || snap.Integrations.Available || snap.Audit.Available {
		t.Fatalf("wedged sub-calls must degrade to unavailable; got %+v", snap)
	}
	// Bounded overall: 5 calls each ~200ms => well under a few seconds.
	if elapsed > 5*time.Second {
		t.Fatalf("fetch did not stay bounded (%v) — TUI would freeze", elapsed)
	}

	// And the model renders the friendly unreachable screen, no panic.
	m := NewModel(func() Snapshot { return snap }, time.Hour)
	updated, _ := m.Update(snapshotMsg{snap: snap})
	out := updated.(Model).View()
	if out == "" {
		t.Fatal("View() must render something for a wedged daemon, not blank")
	}
}

// TestClientClosedYieldsErrorNotPanic ensures a call after Close is a clean
// error (the model would surface unavailable), never a panic.
func TestClientClosedYieldsErrorNotPanic(t *testing.T) {
	path := silentServer(t)
	cl, err := Dial(path, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = cl.Close()
	if _, err := cl.Health(); err == nil {
		t.Fatal("a call on a closed client must error")
	}
}

// slowThenFastServer models a daemon that is UNRESPONSIVE on its first
// connection (forcing the client's first call to breach its read deadline and
// drop the conn) but replies promptly on every subsequent connection. A correct
// read-only client must SELF-HEAL: re-dial on the next poll and succeed — it must
// NOT stay permanently poisoned (a json.Decoder caches a read-deadline error for
// the life of a connection). This is the regression guard for that defect.
func slowThenFastServer(t *testing.T) string {
	t.Helper()
	path := tmpSockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	accepted := 0
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			n := accepted
			accepted++
			mu.Unlock()
			if n == 0 {
				continue // FIRST conn: never reply — the client must time out + drop it
			}
			go serveEcho(conn)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		conns = nil
		mu.Unlock()
	})
	return path
}

// serveEcho replies to each request with a minimal valid JSON-RPC response that
// echoes the request id (a healthy daemon).
func serveEcho(conn net.Conn) {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req protocol.Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		resp := protocol.Response{
			JSONRPC: protocol.JSONRPCVersion,
			V:       protocol.ContractVersion,
			ID:      req.ID,
			Result:  json.RawMessage(`{"pid":1,"contract_version":1}`),
		}
		if err := enc.Encode(&resp); err != nil {
			return
		}
	}
}

// TestClientRecoversAfterTimeout is the SELF-HEAL regression test: after one
// timed-out call (which poisons & drops the connection), a subsequent call must
// re-dial the now-responsive daemon and SUCCEED. Without reconnect-on-error this
// fails forever (the sticky json.Decoder), which is the confirmed 9a defect.
func TestClientRecoversAfterTimeout(t *testing.T) {
	path := slowThenFastServer(t)
	cl, err := Dial(path, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cl.Close()

	// First call hits the unresponsive first connection: it must ERROR within the
	// deadline (not hang), and internally drop the poisoned connection.
	start := time.Now()
	if _, err := cl.Health(); err == nil {
		t.Fatal("first call against an unresponsive daemon must error")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("first call did not respect the read deadline (%v) — TUI would freeze", d)
	}

	// Self-heal: a subsequent call must re-dial and reach the now-responsive
	// daemon. Retry briefly to absorb accept/serve scheduling.
	var lastErr error
	for i := 0; i < 40; i++ {
		if _, err := cl.Health(); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("client did NOT self-heal after a timed-out call (permanent poison): %v", lastErr)
	}
}
