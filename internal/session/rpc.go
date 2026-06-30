package session

import (
	"encoding/base64"
	"encoding/json"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

// RegisterMethods wires the two session-unifier methods onto the daemon registry.
// These are the ONLY additions to the frozen registry (T2). They must be
// registered BEFORE the registry Freeze in compose.
func RegisterMethods(reg *daemon.Registry, s *Store) error {
	for _, m := range []struct {
		name string
		h    daemon.Handler
	}{
		{"session.mint_token", mintTokenHandler(s)},
		{"session.attach", attachHandler(s)},
	} {
		if err := reg.Register(m.name, m.h); err != nil {
			return err
		}
	}
	return nil
}

// mintTokenHandler binds a fresh unforgeable token to the CALLER's connection
// identity (cc.Session) — NEVER a param (Inv 4). Any session id in params is
// ignored. It returns {token, liveness_key} where liveness_key is the base64
// session-liveness lease key the holder acquires+renews; the daemon returns the
// key so the holder never fabricates one (Inv 9).
func mintTokenHandler(s *Store) daemon.Handler {
	return func(cc *daemon.CallContext, _ json.RawMessage) (json.RawMessage, *protocol.Error) {
		token, err := s.Mint(cc.Session) // bound to the connection, not params
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, "token minting failed")
		}
		return jsonResult(map[string]string{
			"token":        token,
			"liveness_key": base64.StdEncoding.EncodeToString(LivenessKey(cc.Session)),
		}), nil
	}
}

// attachHandler rebinds the calling connection to an existing session named ONLY
// indirectly by an unforgeable token. It NEVER accepts a session-id param. An
// unknown/garbage token, or a token whose session is no longer LIVE (its
// session-liveness lease is not currently held by it), is a CodeAuthz fault and
// leaves cc.Session UNCHANGED.
func attachHandler(s *Store) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			Token string `json:"token"`
			// NOTE: there is deliberately NO session field. A client can never name
			// a session id directly (Inv 4) — the token is the sole identity path.
		}
		if len(params) == 0 {
			return nil, protocol.NewError(protocol.CodeInvalidParams, "params required")
		}
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, protocol.NewError(protocol.CodeInvalidParams, "invalid params")
		}
		if in.Token == "" {
			return nil, protocol.NewError(protocol.CodeInvalidParams, "token required")
		}

		sessionID, ok := s.Resolve(in.Token)
		if !ok {
			// Unknown/garbage token: authz fault. cc.Session is NOT mutated.
			return nil, protocol.NewError(protocol.CodeAuthz, "session token refers to a dead/unknown session")
		}

		// Liveness gate: the session is alive ONLY if its session-liveness lease is
		// currently held by that very session. A dead/expired session (lease lapsed
		// or held by someone else) cannot be attached to.
		holder, live, err := s.leases.LiveHolder(LivenessKey(sessionID))
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, "session liveness check failed")
		}
		if !live || holder != string(sessionID) {
			// Do NOT evict here: a session that is not-live AT THIS INSTANT is not
			// necessarily permanently dead (mint precedes the launcher's lease acquire
			// by a brief window), and the token must still attach once the lease is
			// held. Dead tokens are bounded by pruneDeadLocked on the next Mint.
			return nil, protocol.NewError(protocol.CodeAuthz, "session token refers to a dead/unknown session")
		}

		// The SINGLE sanctioned identity mutation (Inv 4 exception, token-gated).
		cc.RebindSession(sessionID)
		return jsonResult(map[string]string{"session": string(sessionID)}), nil
	}
}

func jsonResult(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
