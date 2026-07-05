package session

// Hand-authored invariant tests for the T2 session-identity/liveness UNIFIER.
// Negatives carry the positive control (the same path succeeds when the gate is
// satisfied) so a vacuous pass is impossible.

import (
	"encoding/base64"
	"encoding/json"
	"sync"
	"testing"

	"github.com/madhavhaldia/mad-trellis/internal/daemon"
	"github.com/madhavhaldia/mad-trellis/internal/protocol"
)

// fakeLeases is a controllable read-only lease checker keyed by raw key string.
type fakeLeases struct {
	mu      sync.Mutex
	holders map[string]string // key -> holder (presence => live)
	err     error
}

func newFakeLeases() *fakeLeases { return &fakeLeases{holders: map[string]string{}} }

func (f *fakeLeases) LiveHolder(key []byte) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", false, f.err
	}
	h, ok := f.holders[string(key)]
	return h, ok, nil
}

// hold marks the session-liveness lease for id as held by id (the live case).
func (f *fakeLeases) hold(id daemon.SessionID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holders[string(LivenessKey(id))] = string(id)
}

func mintToken(t *testing.T, s *Store, cc *daemon.CallContext) (token, livenessKey string) {
	t.Helper()
	res, perr := mintTokenHandler(s)(cc, nil)
	if perr != nil {
		t.Fatalf("mint_token: %+v", perr)
	}
	var out struct {
		Token       string `json:"token"`
		LivenessKey string `json:"liveness_key"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("unmarshal mint result: %v", err)
	}
	return out.Token, out.LivenessKey
}

func attach(s *Store, cc *daemon.CallContext, token string) (json.RawMessage, *protocol.Error) {
	params, _ := json.Marshal(map[string]string{"token": token})
	return attachHandler(s)(cc, params)
}

// --- mint -> attach roundtrip rebinds the new connection's identity ----------

func TestMintAttachRoundtripRebinds(t *testing.T) {
	leases := newFakeLeases()
	s := NewStore(leases, NewMemTokenStore())

	holderID := daemon.SessionID("s-1-holder")
	holder := &daemon.CallContext{Session: holderID}
	token, livenessKey := mintToken(t, s, holder)

	// The holder keeps its session alive by holding the liveness lease.
	leases.hold(holderID)

	// liveness_key is the base64 of the canonical key (Inv 9: daemon-returned).
	wantKey := base64.StdEncoding.EncodeToString(LivenessKey(holderID))
	if livenessKey != wantKey {
		t.Fatalf("liveness_key mismatch: got %q want %q", livenessKey, wantKey)
	}

	// A NEW connection (freshly minted distinct identity) attaches with the token.
	newConn := &daemon.CallContext{Session: daemon.SessionID("s-2-newconn")}
	res, perr := attach(s, newConn, token)
	if perr != nil {
		t.Fatalf("attach: %+v", perr)
	}
	if newConn.Session != holderID {
		t.Fatalf("attach must rebind cc.Session to the holder; got %q want %q", newConn.Session, holderID)
	}
	var out struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("unmarshal attach result: %v", err)
	}
	if out.Session != string(holderID) {
		t.Fatalf("attach result session %q != %q", out.Session, holderID)
	}
}

// --- unknown/garbage token -> CodeAuthz, identity UNCHANGED ------------------

func TestAttachUnknownTokenIsAuthzAndPreservesIdentity(t *testing.T) {
	leases := newFakeLeases()
	s := NewStore(leases, NewMemTokenStore())

	orig := daemon.SessionID("s-7-original")
	cc := &daemon.CallContext{Session: orig}
	for _, tok := range []string{"not-a-real-token", base64.StdEncoding.EncodeToString([]byte("garbage-but-base64"))} {
		_, perr := attach(s, cc, tok)
		if perr == nil || perr.Code != protocol.CodeAuthz {
			t.Fatalf("unknown token must be CodeAuthz; got %+v", perr)
		}
		if cc.Session != orig {
			t.Fatalf("a failed attach must NOT mutate cc.Session; got %q want %q", cc.Session, orig)
		}
	}
}

// --- dead session (liveness lease NOT held) -> CodeAuthz --------------------

func TestAttachDeadSessionIsAuthz(t *testing.T) {
	leases := newFakeLeases()
	s := NewStore(leases, NewMemTokenStore())

	holderID := daemon.SessionID("s-3-holder")
	holder := &daemon.CallContext{Session: holderID}
	token, _ := mintToken(t, s, holder)

	// The liveness lease is deliberately NOT held => the session is dead.
	newConn := &daemon.CallContext{Session: daemon.SessionID("s-4-newconn")}
	_, perr := attach(s, newConn, token)
	if perr == nil || perr.Code != protocol.CodeAuthz {
		t.Fatalf("attach to a dead session must be CodeAuthz; got %+v", perr)
	}
	if newConn.Session != daemon.SessionID("s-4-newconn") {
		t.Fatalf("a failed attach must NOT rebind identity; got %q", newConn.Session)
	}

	// Positive control: once the holder holds the liveness lease, the SAME token
	// attaches successfully — proving the negative was not vacuous.
	leases.hold(holderID)
	if _, perr := attach(s, newConn, token); perr != nil {
		t.Fatalf("positive control: attach should succeed once live; got %+v", perr)
	}
	if newConn.Session != holderID {
		t.Fatalf("positive control: expected rebind to %q, got %q", holderID, newConn.Session)
	}
}

// A liveness lease held by SOMEONE ELSE must not satisfy attach (holder must be
// the very session the token names).
func TestAttachLeaseHeldByOtherIsAuthz(t *testing.T) {
	leases := newFakeLeases()
	s := NewStore(leases, NewMemTokenStore())

	holderID := daemon.SessionID("s-5-holder")
	token, _ := mintToken(t, s, &daemon.CallContext{Session: holderID})

	// Put a DIFFERENT holder on the session-liveness key.
	leases.mu.Lock()
	leases.holders[string(LivenessKey(holderID))] = "s-99-impostor"
	leases.mu.Unlock()

	cc := &daemon.CallContext{Session: daemon.SessionID("s-6-newconn")}
	_, perr := attach(s, cc, token)
	if perr == nil || perr.Code != protocol.CodeAuthz {
		t.Fatalf("a lease held by another session must not satisfy attach; got %+v", perr)
	}
}

// --- mint_token binds to the CALLER's session; a param session is ignored ----

func TestMintTokenBindsToCallerNotParams(t *testing.T) {
	leases := newFakeLeases()
	s := NewStore(leases, NewMemTokenStore())

	realID := daemon.SessionID("s-10-real")
	cc := &daemon.CallContext{Session: realID}

	// Feed a forged session id in params; the handler must ignore it entirely.
	forged, _ := json.Marshal(map[string]string{"session": "s-evil-forged"})
	res, perr := mintTokenHandler(s)(cc, forged)
	if perr != nil {
		t.Fatalf("mint_token: %+v", perr)
	}
	var out struct {
		Token       string `json:"token"`
		LivenessKey string `json:"liveness_key"`
	}
	_ = json.Unmarshal(res, &out)

	// The token must resolve to the CALLER's identity, never the forged one.
	got, ok := s.Resolve(out.Token)
	if !ok || got != realID {
		t.Fatalf("token must bind to caller %q; resolved (%q, ok=%v)", realID, got, ok)
	}
	if out.LivenessKey != base64.StdEncoding.EncodeToString(LivenessKey(realID)) {
		t.Fatalf("liveness_key must be the caller's key, not the forged one")
	}
}

// --- tokens are unforgeable: unique + high-entropy + not a guessable format --

func TestTokensUniqueAndUnforgeable(t *testing.T) {
	leases := newFakeLeases()
	s := NewStore(leases, NewMemTokenStore())
	id := daemon.SessionID("s-20-x")

	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		tok, err := s.Mint(id)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token minted: %q", tok)
		}
		seen[tok] = true

		raw, err := base64.StdEncoding.DecodeString(tok)
		if err != nil {
			t.Fatalf("token must be valid base64: %v", err)
		}
		if len(raw) < 32 {
			t.Fatalf("token must carry >=32 bytes of entropy; got %d", len(raw))
		}
		// A token must NOT embed/encode the session id (no guessable format).
		if string(raw) == string(id) || base64Contains(tok, string(id)) {
			t.Fatalf("token leaks the session id (guessable format): %q", tok)
		}
	}
}

func base64Contains(tok, needle string) bool {
	raw, err := base64.StdEncoding.DecodeString(tok)
	if err != nil {
		return false
	}
	return containsSub(string(raw), needle)
}

func containsSub(s, sub string) bool {
	if sub == "" {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- RegisterMethods adds EXACTLY the two new methods, named precisely --------

func TestRegisterMethodsAddsExactlyTwo(t *testing.T) {
	d := daemon.New(daemon.Options{SocketPath: "/tmp/nm-session-reg-test.sock"})
	before := len(d.Registry().Methods())
	if err := RegisterMethods(d.Registry(), NewStore(newFakeLeases(), NewMemTokenStore())); err != nil {
		t.Fatalf("RegisterMethods: %v", err)
	}
	after := d.Registry().Methods()
	if len(after)-before != 2 {
		t.Fatalf("RegisterMethods must add exactly 2 methods; before=%d after=%d", before, len(after))
	}
	want := map[string]bool{"session.mint_token": true, "session.attach": true}
	found := 0
	for _, m := range after {
		if want[m] {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("expected session.mint_token and session.attach registered; got %v", after)
	}
}

// --- pruneDeadLocked (on Mint) bounds the table: a dead session's token is dropped.
func TestMintPrunesDeadTokens(t *testing.T) {
	old := pruneGrace
	pruneGrace = 0 // immediate prune (no grace) for a deterministic assertion
	defer func() { pruneGrace = old }()

	leases := newFakeLeases()
	s := NewStore(leases, NewMemTokenStore())

	deadID := daemon.SessionID("s-dead")
	deadTok, err := s.Mint(deadID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	leases.hold(deadID) // the session is briefly live
	if _, ok := s.Resolve(deadTok); !ok {
		t.Fatal("token should resolve while present")
	}
	// The session dies: its session-liveness lease is no longer held.
	leases.mu.Lock()
	delete(leases.holders, string(LivenessKey(deadID)))
	leases.mu.Unlock()

	// A new mint (a different, live session) triggers pruneDeadLocked, which drops
	// the dead session's token — bounding the table to live sessions.
	if _, err := s.Mint(daemon.SessionID("s-live")); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := s.Resolve(deadTok); ok {
		t.Fatal("a dead session's token must be pruned on the next mint")
	}
}

// --- P0 #4: attach survives a daemon RESTART (durable token store) -------------
// A daemon restart drops the in-memory session registry but the DURABLE TokenStore
// (and the durable lease ledger) persist. A FRESH Store over the SAME durable
// backing must still resolve a token minted before the "restart" and attach to its
// (still-live) session — the basis for the launcher re-attaching after a restart.
func TestAttachSurvivesDaemonRestart(t *testing.T) {
	durable := NewMemTokenStore() // the durable backing survives the "restart"
	leases := newFakeLeases()     // the durable lease ledger likewise survives

	holderID := daemon.SessionID("s-1-survivor")
	token, _ := mintToken(t, NewStore(leases, durable), &daemon.CallContext{Session: holderID})
	leases.hold(holderID) // the session-liveness lease is held (durable) and still live

	// "Restart": a brand-new Store with an EMPTY in-memory state, over the SAME
	// durable token + lease backing.
	restarted := NewStore(leases, durable)

	// The token still resolves (durable) and attach rebinds a fresh connection.
	newConn := &daemon.CallContext{Session: daemon.SessionID("s-2-postrestart")}
	if _, perr := attach(restarted, newConn, token); perr != nil {
		t.Fatalf("attach after restart must succeed via the durable token store; got %+v", perr)
	}
	if newConn.Session != holderID {
		t.Fatalf("post-restart attach must rebind to the original identity %q; got %q", holderID, newConn.Session)
	}

	// And the token is stored ONLY as a hash (never the raw bearer secret at rest).
	if _, ok, _ := durable.Get(token); ok {
		t.Fatal("the RAW token must not be a key in the durable store (only its hash)")
	}
	if _, ok, _ := durable.Get(hashToken(token)); !ok {
		t.Fatal("the token HASH must be the durable key")
	}
}

// --- P0 #4: the prune grace protects a freshly-minted (not-yet-live) token ------
// mint PRECEDES the launcher's lease acquire; within the grace window a young
// token whose session is not-yet-live must NOT be pruned (else attach would fail
// for a session that is about to come live).
func TestMintGraceProtectsYoungToken(t *testing.T) {
	leases := newFakeLeases() // no holder held → every session looks "not live"
	s := NewStore(leases, NewMemTokenStore())

	youngID := daemon.SessionID("s-young")
	youngTok, err := s.Mint(youngID) // not-yet-live (no lease held), created NOW
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// A second mint runs prune; with the default grace the young token is SKIPPED.
	if _, err := s.Mint(daemon.SessionID("s-other")); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := s.Resolve(youngTok); !ok {
		t.Fatal("a freshly-minted (young) token must survive prune so its session can attach once live")
	}
}
