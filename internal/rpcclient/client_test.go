package rpcclient

// Hardening regression tests (chafe C9 + C22): the shared RPC client must never
// freeze a CLI on a wedged daemon, must SELF-HEAL after a transient
// slowness/outage (a json.Decoder that hit a read deadline caches that error for
// the life of the connection, so the conn must be dropped + re-dialed), and must
// never mis-attribute a stale late reply on a reused connection to a later Call.
//
// Shared server state is guarded by a mutex + slice (never a channel) to avoid
// the data race the watch test originally had.

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

// TestDefaultReadTimeoutIsGenerous guards against re-tightening the per-call
// deadline below the system's max single-operation budget. The integrator budgets
// a promote to a 120s lease TTL and runs git merge-tree/commit-tree/update-ref
// SYNCHRONOUSLY inside the RPC round-trip; a default deadline shorter than that
// would false-time-out a legitimately slow promote (the regression this floor
// prevents). Snappy wedged-daemon detection is the job of WithReadTimeout or a
// caller's own client (e.g. the watch TUI), NOT the shared default.
func TestDefaultReadTimeoutIsGenerous(t *testing.T) {
	const promoteBudget = 120 * time.Second
	if DefaultReadTimeout < promoteBudget {
		t.Errorf("DefaultReadTimeout=%s is below the promote-lease budget %s; a slow integrate.promote would false-time-out (regression)", DefaultReadTimeout, promoteBudget)
	}
}

// tmpSockPath returns a short, unused Unix socket path that is cleaned up.
func tmpSockPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "nm-rpc-*.sock")
	if err != nil {
		t.Fatalf("tmp socket: %v", err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	t.Cleanup(func() { _ = os.Remove(name) })
	return name
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
			Result:  json.RawMessage(`{"ok":true}`),
		}
		if err := enc.Encode(&resp); err != nil {
			return
		}
	}
}

// silentServer accepts connections but NEVER replies (the wedged-daemon case).
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

func TestCallTimesOutOnWedgedDaemon(t *testing.T) {
	path := silentServer(t)
	cl, err := Dial(path, WithReadTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("dial (accept succeeds): %v", err)
	}
	defer cl.Close()

	start := time.Now()
	err = cl.Call("diag.health", nil, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a wedged daemon must yield an ERROR, not a successful call")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("call did not respect the read deadline (took %v) — CLI would freeze", elapsed)
	}
}

// TestCallWithReadTimeoutBoundsPerCall proves the P0 #4 keepalive fix: even on a
// client with the GENEROUS default read timeout (so provision/teardown are not
// false-timed-out), a SINGLE call can be bounded SHORT via CallWithReadTimeout — so a
// wedged daemon cannot stall the launcher keepalive's renew/reattach past the lease
// TTL / clean-exit budget.
func TestCallWithReadTimeoutBoundsPerCall(t *testing.T) {
	path := silentServer(t)
	cl, err := Dial(path) // DEFAULT (120s) timeout — must NOT govern the bounded call
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cl.Close()

	start := time.Now()
	err = cl.CallWithReadTimeout("session.attach", map[string]any{"token": "x"}, nil, 250*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a wedged daemon must yield an ERROR, not a successful bounded call")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("CallWithReadTimeout did not bound the call (took %v) — a wedged daemon would defeat keepalive recovery", elapsed)
	}
}

func TestDialRefusedOnDeadSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.sock")
	start := time.Now()
	_, err := Dial(path, WithDialTimeout(200*time.Millisecond))
	if err == nil {
		t.Fatal("dialing a dead socket must error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("dial to a dead socket hung: %v", time.Since(start))
	}
}

// slowThenFastServer models a daemon that is UNRESPONSIVE on its first
// connection (forcing the first Call to breach its read deadline + drop the
// conn) but replies promptly on every subsequent connection. The client must
// SELF-HEAL: re-dial on the next Call and succeed — never permanently poisoned.
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

// TestCallRecoversAfterTimeout is the SELF-HEAL regression test: after one
// timed-out Call (which poisons + drops the connection), a subsequent Call must
// re-dial the now-responsive daemon and SUCCEED. Without reconnect-on-error this
// fails forever (the sticky json.Decoder) — the C9/C22 defect.
func TestCallRecoversAfterTimeout(t *testing.T) {
	path := slowThenFastServer(t)
	cl, err := Dial(path, WithReadTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cl.Close()

	start := time.Now()
	if err := cl.Call("diag.health", nil, nil); err == nil {
		t.Fatal("first call against an unresponsive daemon must error")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("first call did not respect the read deadline (%v) — CLI would freeze", d)
	}

	// Self-heal: a subsequent Call must re-dial and reach the now-responsive
	// daemon. Retry briefly to absorb accept/serve scheduling.
	var lastErr error
	for i := 0; i < 40; i++ {
		var out struct {
			OK bool `json:"ok"`
		}
		if err := cl.Call("diag.health", nil, &out); err == nil {
			if !out.OK {
				t.Fatalf("recovered call returned but result not unmarshaled: %+v", out)
			}
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

// TestStaleReplyNotMisattributedOnReusedConn proves response-id correlation:
// a server that, on a single long-lived connection, emits a LATE reply carrying
// the PRIOR request's id followed by the CURRENT request's reply, must not cause
// the current Call to accept the stale frame. The client skips the non-matching
// id and returns the correct payload.
func TestStaleReplyNotMisattributedOnReusedConn(t *testing.T) {
	path := tmpSockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		mu.Lock()
		conns = append(conns, conn)
		mu.Unlock()

		dec := json.NewDecoder(conn)
		enc := json.NewEncoder(conn)
		var req protocol.Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		// Emit a STALE frame first: a response whose id is one LESS than the
		// request we just received (as if a prior timed-out call's reply arrived
		// late on this reused conn), then the correct reply.
		var reqID int
		_ = json.Unmarshal(*req.ID, &reqID)
		staleID := json.RawMessage([]byte("999999"))
		stale := protocol.Response{
			JSONRPC: protocol.JSONRPCVersion, V: protocol.ContractVersion,
			ID:     &staleID,
			Result: json.RawMessage(`{"stale":true,"ok":false}`),
		}
		good := protocol.Response{
			JSONRPC: protocol.JSONRPCVersion, V: protocol.ContractVersion,
			ID:     req.ID,
			Result: json.RawMessage(`{"ok":true}`),
		}
		_ = enc.Encode(&stale)
		_ = enc.Encode(&good)
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

	cl, err := Dial(path, WithReadTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cl.Close()

	var out struct {
		OK    bool `json:"ok"`
		Stale bool `json:"stale"`
	}
	if err := cl.Call("diag.health", nil, &out); err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.Stale {
		t.Fatal("stale frame was mis-attributed to this Call — id correlation failed")
	}
	if !out.OK {
		t.Fatalf("expected the correct (id-matched) reply, got %+v", out)
	}
}

// restartableServer can be told to stop accepting (simulating a daemon restart)
// and a fresh listener can be brought up on the SAME socket path. Tracks conns
// under a mutex (no channel) to avoid a data race.
func TestReconnectAfterServerRestart(t *testing.T) {
	path := tmpSockPath(t)

	start := func() (net.Listener, func()) {
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
				conns = append(conns, conn)
				mu.Unlock()
				go serveEcho(conn)
			}
		}()
		stop := func() {
			_ = ln.Close()
			mu.Lock()
			for _, c := range conns {
				_ = c.Close()
			}
			conns = nil
			mu.Unlock()
		}
		return ln, stop
	}

	_, stop1 := start()

	cl, err := Dial(path, WithReadTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cl.Close()

	// First call succeeds against the original server.
	if err := cl.Call("diag.health", nil, nil); err != nil {
		t.Fatalf("initial call: %v", err)
	}

	// "Restart": stop the server (drops the client's conn on the next read) and
	// bring up a fresh one on the same socket path.
	stop1()
	_ = os.Remove(path)

	// Next call may error (the dropped conn) — the client must drop it and the
	// re-dial then reaches the new server. Bring the new server up and retry.
	_, stop2 := start()
	t.Cleanup(stop2)

	var lastErr error
	for i := 0; i < 40; i++ {
		if err := cl.Call("diag.health", nil, nil); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("client did not reconnect after server restart: %v", lastErr)
	}
}

// TestCallAfterCloseErrors confirms a torn-down client never re-dials.
func TestCallAfterCloseErrors(t *testing.T) {
	path := silentServer(t)
	cl, err := Dial(path, WithReadTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = cl.Close()
	if err := cl.Call("diag.health", nil, nil); err == nil {
		t.Fatal("a call on a closed client must error")
	}
}

// TestBackwardCompatDialSignature ensures the zero-option Dial(socket) form the
// CLIs use still compiles and works with the default timeouts.
func TestBackwardCompatDialSignature(t *testing.T) {
	path := tmpSockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		serveEcho(conn)
	}()

	cl, err := Dial(path) // exact legacy call-site form
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cl.Close()
	var out struct {
		OK bool `json:"ok"`
	}
	if err := cl.Call("diag.health", nil, &out); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !out.OK {
		t.Fatalf("unexpected result: %+v", out)
	}
}
