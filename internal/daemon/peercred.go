package daemon

import (
	"net"
	"os"
)

// Phase 4 defense-in-depth: a same-uid LOCAL_PEERCRED check on the accept path so
// the identity trust root no longer rests on the 0600 socket file permission
// ALONE (see acquireSocket and identity.go's note about the swappable trust
// root). The file-perm gate stays the PRIMARY control; this peer-credential
// check is an additional, independent layer.
//
// It is intentionally FAIL-OPEN: a connection is rejected ONLY on a DEFINITIVE
// uid mismatch (the peer uid can be read AND differs from this process's uid).
// On any uncertainty — a non-Unix connection, no syscall access to the fd, or a
// failed peer-credential lookup — the connection is ALLOWED, so a legitimate
// same-uid client is never broken by this hardening.

// peerUIDOf returns the OS uid of the connected peer for a Unix-socket
// connection. The bool is false (and the uid meaningless) whenever the uid
// cannot be determined: a non-*net.UnixConn, no raw syscall access, or a
// platform/syscall failure in the per-GOOS peerUID lookup.
func peerUIDOf(conn net.Conn) (uint32, bool) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false // not a Unix socket: nothing to check
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var (
		uid uint32
		got bool
	)
	if cerr := raw.Control(func(fd uintptr) {
		uid, got = peerUID(fd)
	}); cerr != nil {
		return 0, false
	}
	return uid, got
}

// peerUIDMismatch is the PURE admission decision, extracted from the I/O so the
// REJECT branch is directly and non-vacuously unit-testable (no real cross-uid
// socket required). It reports whether the peer is DEFINITIVELY a different OS
// user than the daemon: true ONLY when the peer uid was readable (ok) AND differs
// from daemonUID. When ok==false (peer creds undeterminable) it returns false —
// fail-OPEN, deferring to the file-perm gate — and equal uids return false (allow).
func peerUIDMismatch(peerUID uint32, ok bool, daemonUID int) bool {
	if !ok {
		return false // could not read peer creds: fail OPEN, defer to the file-perm gate
	}
	return int(peerUID) != daemonUID
}

// peerConnMismatch wires the pure decision to a live connection: it reads the
// peer uid off conn and evaluates it against this process's uid. On ANY
// uncertainty (non-Unix conn, no syscall access, lookup failure) peerUIDOf
// yields ok==false and the decision fails OPEN.
func peerConnMismatch(conn net.Conn) bool {
	uid, ok := peerUIDOf(conn)
	return peerUIDMismatch(uid, ok, os.Getuid())
}
