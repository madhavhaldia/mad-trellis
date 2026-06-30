package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

// SessionID is a daemon-minted, connection-bound identity. It is unforgeable by
// a client: the daemon assigns it on accept and never reads identity from a
// request payload (Inv 4 — enforce at the boundary, never trust).
type SessionID string

// Authenticator establishes the session identity for an accepted connection. It
// is the SWAPPABLE trust root (docs/0004 open issue #1): the v1 default mints a
// per-connection identity and relies on Unix-socket file permissions (0600,
// owner-only) for OS-level admission; a peer-credential (LOCAL_PEERCRED uid)
// check is the documented swap-in and is intentionally behind this interface.
type Authenticator interface {
	Authenticate(conn net.Conn) (SessionID, error)
}

// mintedAuth mints a unique, unguessable SessionID per connection. The
// monotonic sequence guarantees N connections yield N distinct identities; the
// random suffix makes an identity unguessable. Identity is bound to the
// accepted connection, never to client-supplied data.
type mintedAuth struct {
	seq atomic.Uint64
}

func (a *mintedAuth) Authenticate(conn net.Conn) (SessionID, error) {
	n := a.seq.Add(1)
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return SessionID(fmt.Sprintf("s-%d-%s", n, hex.EncodeToString(b[:]))), nil
}

// AuditRecord is the FROZEN, append-only decision-audit record contract. Every
// governance decision is recorded through the daemon's audit interface. Storage
// is implemented by the lease ledger (project 2) BEHIND this interface so the
// lease store never becomes a coupling hub (docs/0003 L162); the daemon owns
// only the write-interface and this record shape.
type AuditRecord struct {
	Timestamp       time.Time       `json:"timestamp"`
	Session         SessionID       `json:"session"`
	DecisionProject string          `json:"decision_project"`
	DecisionKind    string          `json:"decision_kind"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

// AuditSink is the append-only storage seam. There is deliberately NO update or
// delete method — append-only. The default is NoopSink; project 2 installs the
// durable SQLite-backed sink behind the same interface.
type AuditSink interface {
	Append(rec AuditRecord) error
}

// AuditReader is the READ-ONLY companion seam to AuditSink (project 9a's
// watch-view surface). It exposes a single bounded tail query — newest-first,
// capped — and performs ZERO writes. It is deliberately separate from AuditSink
// so a write-only emitter never gains a read path and the read-only watch client
// never gains an append path. The same durable lease-ledger sink implements BOTH
// (it is the single storage), but the daemon holds each behind its own optional
// field so a minimally-composed daemon can install one without the other.
type AuditReader interface {
	// Tail returns up to limit records, NEWEST-FIRST. A non-positive limit means
	// "use a sensible default". Implementations perform SELECT-only SQL.
	Tail(limit int) ([]AuditRecord, error)
}

// NoopSink discards records. It lets every decision emitter code and test
// against the frozen interface before durable storage exists (the P0 no-op
// audit sink stub).
type NoopSink struct{}

// Append implements AuditSink.
func (NoopSink) Append(AuditRecord) error { return nil }
