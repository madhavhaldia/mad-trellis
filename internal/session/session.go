// Package session implements the T2 session-identity/liveness UNIFIER: it lets
// all of one agent's processes (launcher + adapter MCP server + hooks) share ONE
// daemon session identity via an UNFORGEABLE, daemon-minted capability token.
//
// Two RPC methods are added (the only additions to the frozen registry):
//   - session.mint_token: a session HOLDER (e.g. the launcher on its held
//     connection) asks the daemon to mint a token bound to the CALLER's
//     connection identity (never a param). The daemon stores token->sessionID and
//     returns {token, liveness_key} where liveness_key is the base64 lease key
//     the launcher acquires+renews to keep the session LIVE (returned by the
//     daemon so the launcher never FABRICATES a key — Inv 9).
//   - session.attach: a NEW connection presents {token}; the daemon resolves it to
//     a sessionID, confirms that session is still LIVE (its session-liveness lease
//     is currently held by it), then REBINDS this connection to that identity via
//     the single sanctioned mutation (cc.RebindSession). A client can NEVER name a
//     session id directly (Inv 4): the token is the only identity path, and it is
//     unforgeable (crypto/rand, >=32 bytes).
//
// DURABILITY (P0 #4 — re-attach across a daemon restart): the token->sessionID
// binding is held in a DURABLE TokenStore so session.attach SURVIVES a daemon
// restart. Without it, a daemon restart drops the in-memory table, the launcher
// cannot re-attach (its token resolves to nothing), the session-liveness lease
// lapses, and liveness reclaims a STILL-RUNNING session's boundary. The launcher
// knows its own session id (it is encoded in the liveness_key), but a re-attach
// needs an UNFORGEABLE proof checkable against durable state — the lease fence is
// guessable (starts at 1), so only the opaque token works. The store persists the
// token HASH (never the raw bearer secret) via an INTERFACE so this package gains
// no direct write edge to the lease store; compose backs it with the ledger.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sync"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
)

// sessionLeaseKeyPrefix is the v1 session-liveness lease key namespace. The
// per-session liveness lease key is sessionLeaseKeyPrefix + <sessionID>; its TTL
// expiry is the ONE TRUE session-death signal (a normal durable ledger lease the
// launcher acquires and renews). The format mirrors the manifest's key
// conventions (e.g. "mad-substrate:trunk:v1").
const sessionLeaseKeyPrefix = "mad-substrate:session:v1:"

// LivenessKey returns the (raw) session-liveness lease key for a session id. The
// daemon returns its base64 form from mint_token so the launcher never fabricates
// a key (Inv 9), and attach derives the same key to inspect liveness.
func LivenessKey(id daemon.SessionID) []byte {
	return []byte(sessionLeaseKeyPrefix + string(id))
}

// LivenessKeyPrefix returns the (raw) "mad-substrate:session:v1:" prefix every
// session-liveness lease key begins with. Liveness (project 8) keys boundary
// recovery off an expired lease under this prefix being the canonical session-
// death signal (T2); compose passes it so the SAME format anchors minting,
// attach, and recovery. Returns a fresh copy so callers cannot mutate it.
func LivenessKeyPrefix() []byte {
	return []byte(sessionLeaseKeyPrefix)
}

// SessionLeaseChecker is the read-only lease seam session.attach uses to confirm
// a session is still LIVE. It is deliberately read-only (no acquire/renew/release/
// reclaim) so this package can never mutate the ledger — compose backs it with a
// thin adapter over the real lease ledger's single-key inspect.
type SessionLeaseChecker interface {
	// LiveHolder reports the current holder of key and whether the lease is LIVE
	// (holder present AND not expired). A missing key is (holder="", live=false,
	// nil) — absence is not an error.
	LiveHolder(key []byte) (holder string, live bool, err error)
}

// TokenStore is the DURABLE backing for the capability-token table: a token HASH
// -> sessionID binding (with its creation time, for the prune grace window). It is
// durable so session.attach survives a daemon restart (P0 #4). The session package
// takes it as an INTERFACE so it never imports/writes the lease store directly;
// compose backs it with the ledger. ONLY the token hash is stored (never the raw
// bearer secret), so a ledger leak does not yield usable tokens.
type TokenStore interface {
	// Put records hash->sessionID (createdMs = unix-millis). An existing hash is
	// overwritten (the same session re-minting), which is benign.
	Put(tokenHash, sessionID string, createdMs int64) error
	// Get returns the sessionID for a hash (ok=false if absent). An absent hash is
	// not an error.
	Get(tokenHash string) (sessionID string, ok bool, err error)
	// Delete removes a hash binding (idempotent).
	Delete(tokenHash string) error
	// List returns every binding (for prune). Order is unspecified.
	List() ([]TokenBinding, error)
}

// TokenBinding is one durable token row.
type TokenBinding struct {
	TokenHash string
	SessionID string
	CreatedMs int64
}

// Store is the capability-token table: token hash -> bound sessionID, persisted in
// a durable TokenStore so attach survives a daemon restart. Mint/prune are
// mutex-guarded so a single daemon's (rare) mints serialize.
type Store struct {
	leases SessionLeaseChecker
	tokens TokenStore

	mu sync.Mutex
}

// NewStore constructs a token store backed by the given read-only lease checker and
// durable token store. For tests, pass NewMemTokenStore() as the durable backing.
func NewStore(leases SessionLeaseChecker, tokens TokenStore) *Store {
	return &Store{leases: leases, tokens: tokens}
}

// tokenBytes is the capability-token length: 32 bytes of crypto/rand entropy,
// base64-encoded on the wire. >=32 bytes makes a token unguessable (Inv 4: the
// only identity path is unforgeable).
const tokenBytes = 32

// pruneGrace keeps a freshly-minted token from being pruned before the launcher
// has acquired its session-liveness lease (mint PRECEDES acquire by a brief
// window, during which the session is not-yet-live). Generous: the real window is
// milliseconds; this never lets a live session's token vanish. A package var so a
// test can drive immediate-prune behavior deterministically.
var pruneGrace = 5 * time.Minute

// hashToken returns the at-rest identifier for a raw bearer token (sha256, hex).
// Storing only the hash means a ledger leak cannot reveal usable tokens.
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func nowMs() int64 { return time.Now().UnixNano() / int64(time.Millisecond) }

// Mint generates a fresh, unforgeable, unique token bound to id and records its
// HASH durably. The caller (the mint_token handler) passes cc.Session — NEVER a
// param — as id. Generation uses crypto/rand and retries on the (astronomically
// unlikely) collision so two mints always differ and a token is never reused.
func (s *Store) Mint(id daemon.SessionID) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneDeadLocked() // bound the durable table: drop tokens whose session is dead
	for {
		var b [tokenBytes]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		tok := base64.StdEncoding.EncodeToString(b[:])
		h := hashToken(tok)
		if _, dup, err := s.tokens.Get(h); err != nil {
			return "", err
		} else if dup {
			continue // unique: never overwrite an existing binding
		}
		if err := s.tokens.Put(h, string(id), nowMs()); err != nil {
			return "", err
		}
		return tok, nil
	}
}

// Resolve looks up the sessionID bound to a token (via its hash). ok=false for an
// unknown token OR a durable read error (fail-closed: an unverifiable token is
// treated as unknown, so attach rejects it).
func (s *Store) Resolve(token string) (daemon.SessionID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok, err := s.tokens.Get(hashToken(token))
	if err != nil || !ok {
		return "", false
	}
	return daemon.SessionID(id), true
}

// pruneDeadLocked drops every durable token whose session-liveness lease is no
// longer held by its session (its session is dead), bounding the table to live
// sessions. A token created within pruneGrace is SKIPPED (its session may not have
// acquired its lease yet — mint precedes acquire). Best-effort: a list/lease-check
// error leaves entries (attach still gates on liveness). The caller holds s.mu.
func (s *Store) pruneDeadLocked() {
	rows, err := s.tokens.List()
	if err != nil {
		return
	}
	cutoff := nowMs() - pruneGrace.Milliseconds()
	for _, r := range rows {
		if r.CreatedMs > cutoff {
			continue // young: its session may not be live yet
		}
		holder, live, err := s.leases.LiveHolder(LivenessKey(daemon.SessionID(r.SessionID)))
		if err == nil && (!live || holder != r.SessionID) {
			_ = s.tokens.Delete(r.TokenHash)
		}
	}
}

// memTokenStore is an in-memory TokenStore for tests (and any non-durable
// deployment). Production wires the durable ledger-backed store via compose.
type memTokenStore struct {
	mu sync.Mutex
	m  map[string]TokenBinding
}

// NewMemTokenStore returns an in-memory TokenStore (test/fallback backing).
func NewMemTokenStore() TokenStore { return &memTokenStore{m: map[string]TokenBinding{}} }

func (s *memTokenStore) Put(h, id string, createdMs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[h] = TokenBinding{TokenHash: h, SessionID: id, CreatedMs: createdMs}
	return nil
}

func (s *memTokenStore) Get(h string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[h]
	return b.SessionID, ok, nil
}

func (s *memTokenStore) Delete(h string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, h)
	return nil
}

func (s *memTokenStore) List() ([]TokenBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TokenBinding, 0, len(s.m))
	for _, b := range s.m {
		out = append(out, b)
	}
	return out, nil
}
