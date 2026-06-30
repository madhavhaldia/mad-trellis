package singular

import (
	"encoding/json"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

// RegisterMethods wires the gate onto the daemon's JSON-RPC registry. The grant
// HOLDER is bound to the connection session (Inv 4): request/release act as the
// caller's daemon-minted identity, so the supervised grant lease is genuinely the
// caller's and one session cannot release another's. The decision is
// deterministic (Inv 2(b)); no probabilistic component appears here.
func RegisterMethods(reg *daemon.Registry, g *Gate) error {
	for _, m := range []struct {
		name string
		h    daemon.Handler
	}{
		{"singular.resolve", resolveHandler(g)},
		{"singular.request", requestHandler(g)},
		{"singular.renew", renewHandler(g)},
		{"singular.release", releaseHandler(g)},
	} {
		if err := reg.Register(m.name, m.h); err != nil {
			return err
		}
	}
	return nil
}

func resolveHandler(g *Gate) daemon.Handler {
	return func(_ *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		res, perr := decodeResource(params)
		if perr != nil {
			return nil, perr
		}
		r := g.Resolve(res)
		return result(map[string]any{"resource": r.Resource, "singular": r.Singular, "mode": r.Mode.String(), "reason": r.Reason}), nil
	}
}

func requestHandler(g *Gate) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		res, perr := decodeResource(params)
		if perr != nil {
			return nil, perr
		}
		a, err := g.Request(string(cc.Session), res)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return result(map[string]any{
			"resource": a.Resource, "mode": a.Mode.String(), "granted": a.Granted,
			"real_reachable": a.RealReachable, "env": a.Env, "reason": a.Reason,
		}), nil
	}
}

func renewHandler(g *Gate) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		res, perr := decodeResource(params)
		if perr != nil {
			return nil, perr
		}
		ok, err := g.Renew(string(cc.Session), res)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return result(map[string]any{"ok": ok}), nil
	}
}

func releaseHandler(g *Gate) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		res, perr := decodeResource(params)
		if perr != nil {
			return nil, perr
		}
		ok, err := g.ReleaseSupervised(string(cc.Session), res)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return result(map[string]any{"ok": ok}), nil
	}
}

func decodeResource(params json.RawMessage) (string, *protocol.Error) {
	var in struct {
		Resource string `json:"resource"`
	}
	if len(params) == 0 {
		return "", protocol.NewError(protocol.CodeInvalidParams, "params required")
	}
	if err := json.Unmarshal(params, &in); err != nil {
		return "", protocol.NewError(protocol.CodeInvalidParams, "invalid params")
	}
	if in.Resource == "" {
		return "", protocol.NewError(protocol.CodeInvalidParams, "resource required")
	}
	return in.Resource, nil
}

func result(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
