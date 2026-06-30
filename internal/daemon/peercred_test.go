package daemon

// Phase 4 defense-in-depth: the same-uid LOCAL_PEERCRED check on the accept path.
// These tests run over a REAL unix listener + dial (not a mock) so the peer
// credential lookup exercises the actual kernel path.

import (
	"net"
	"os"
	"runtime"
	"testing"
	"time"
)

// unixConnPair returns the SERVER-side and CLIENT-side ends of a real Unix-socket
// connection. Both ends are owned by this test process, hence the same uid.
func unixConnPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	path := tmpSock(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	errc := make(chan error, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			errc <- aerr
			return
		}
		accepted <- c
	}()

	client, err = net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	select {
	case server = <-accepted:
	case err = <-errc:
		t.Fatalf("accept: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out accepting the unix connection")
	}
	t.Cleanup(func() { _ = server.Close() })
	return server, client
}

// TestPeerUIDExtractionSameUID proves the peer-credential extraction is REAL
// (non-vacuous) on the supported platforms: it reads the actual peer uid and it
// equals this process's uid. On unsupported platforms it intentionally yields
// ok=false (fail-open). Either way the same-uid connection must be ALLOWED.
func TestPeerUIDExtractionSameUID(t *testing.T) {
	server, _ := unixConnPair(t)

	uid, ok := peerUIDOf(server)
	switch runtime.GOOS {
	case "darwin", "linux":
		if !ok {
			t.Fatalf("peer uid extraction must succeed on %s (LOCAL_PEERCRED/SO_PEERCRED)", runtime.GOOS)
		}
		if int(uid) != os.Getuid() {
			t.Fatalf("peer uid %d must equal this process uid %d for a self-dialed socket", uid, os.Getuid())
		}
	default:
		if ok {
			t.Fatalf("peer uid extraction unexpectedly succeeded on unsupported GOOS %s", runtime.GOOS)
		}
	}

	if peerConnMismatch(server) {
		t.Fatal("a same-uid peer must be ALLOWED (defense-in-depth must never break a legit same-uid client)")
	}
}

// TestPeerUIDMismatchFailsOpenOnNonUnix proves the fail-OPEN posture: a
// connection whose peer uid cannot be determined (here, a non-Unix conn) is
// ALLOWED, never rejected.
func TestPeerUIDMismatchFailsOpenOnNonUnix(t *testing.T) {
	a, b := net.Pipe() // not a *net.UnixConn: uid is undeterminable
	defer a.Close()
	defer b.Close()

	if _, ok := peerUIDOf(a); ok {
		t.Fatal("a non-Unix conn must not yield a peer uid")
	}
	if peerConnMismatch(a) {
		t.Fatal("an undeterminable peer uid must FAIL OPEN (allow), not reject")
	}
}

// TestPeerUIDMismatchDecision exercises the PURE admission decision directly,
// including the REJECT branch that the real same-uid socket path can never reach
// in-process. This is the NON-VACUOUS control for the LOCAL_PEERCRED security
// clause: it proves the check actually flips to REJECT on a forged cross-uid peer.
func TestPeerUIDMismatchDecision(t *testing.T) {
	const daemonUID = 1000

	// REJECT: peer uid readable AND different from the daemon uid.
	if !peerUIDMismatch(1001, true, daemonUID) {
		t.Fatal("a definitive cross-uid peer (ok=true, uid != daemon) must REJECT")
	}
	// ALLOW: peer uid readable AND equal to the daemon uid.
	if peerUIDMismatch(daemonUID, true, daemonUID) {
		t.Fatal("a same-uid peer (ok=true, uid == daemon) must be ALLOWED")
	}
	// FAIL-OPEN: peer uid undeterminable (ok=false) — allow even though the
	// (meaningless) uid happens to differ, proving ok==false short-circuits.
	if peerUIDMismatch(1001, false, daemonUID) {
		t.Fatal("an undeterminable peer uid (ok=false) must FAIL OPEN regardless of the uid value")
	}
	// FAIL-OPEN holds even when the meaningless uid equals the daemon uid.
	if peerUIDMismatch(daemonUID, false, daemonUID) {
		t.Fatal("ok==false must fail open irrespective of the uid value")
	}
}

// TestSameUIDConnectionServedEndToEnd drives the full accept path of a live
// daemon: a same-uid client (this process) must pass the peercred check and get
// a normal minted identity back, not an authz rejection.
func TestSameUIDConnectionServedEndToEnd(t *testing.T) {
	_, path := startDaemon(t, nil)
	c := dialClient(t, path)
	defer c.close()
	var got struct {
		Session string `json:"session"`
	}
	mustResult(t, c.call(t, "session.whoami", nil), &got)
	if got.Session == "" {
		t.Fatal("a same-uid client must be served (peercred check must not reject it)")
	}
}
