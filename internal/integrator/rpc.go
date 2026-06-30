package integrator

import (
	"encoding/json"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

// RegisterMethods wires the integrator onto the daemon's JSON-RPC registry — the
// frozen external integration surface. The integration HOLDER and the promoting
// caller are bound to the connection session (Inv 4): submit/promote act as the
// caller's daemon-minted identity, never an id taken from params, so the trunk
// lease the promote acquires is genuinely the caller's. The promote/gate path is
// deterministic (Inv 2(b)); no probabilistic component appears here.
func RegisterMethods(reg *daemon.Registry, it *Integrator) error {
	for _, m := range []struct {
		name string
		h    daemon.Handler
	}{
		{"integrate.submit", submitHandler(it)},
		{"integrate.promote", promoteHandler(it)},
		{"integrate.abort", abortHandler(it)},
		{"integrate.status", statusHandler(it)},
		{"integrate.list", listHandler(it)},
		{"integrate.trunk", trunkHandler(it)},
	} {
		if err := reg.Register(m.name, m.h); err != nil {
			return err
		}
	}
	return nil
}

func submitHandler(it *Integrator) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			Branch string `json:"branch"`
		}
		if perr := decode(params, &in); perr != nil {
			return nil, perr
		}
		rec, err := it.Submit(string(cc.Session), in.Branch)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInvalidParams, err.Error())
		}
		return result(map[string]any{"id": rec.ID, "state": string(rec.State), "base": rec.Base}), nil
	}
}

func promoteHandler(it *Integrator) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			ID string `json:"id"`
		}
		if perr := decode(params, &in); perr != nil {
			return nil, perr
		}
		out, err := it.Promote(string(cc.Session), in.ID)
		if err != nil {
			if err == ErrNotFound {
				return nil, protocol.NewError(protocol.CodeNotFound, err.Error())
			}
			if err == ErrAborted {
				return nil, protocol.NewError(protocol.CodeConflict, err.Error())
			}
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return outcome(out), nil
	}
}

func abortHandler(it *Integrator) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			ID string `json:"id"`
		}
		if perr := decode(params, &in); perr != nil {
			return nil, perr
		}
		// HOLDER-BOUND (Inv 4): only the submitting session may abort its own
		// in-flight integration over RPC — one client cannot cancel another's. The
		// privileged, non-holder-bound Abort(id) is reserved for in-process liveness.
		out, err := it.AbortAs(string(cc.Session), in.ID)
		if err != nil {
			if err == ErrUnauthorized {
				return nil, protocol.NewError(protocol.CodeAuthz, err.Error())
			}
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return outcome(out), nil
	}
}

func statusHandler(it *Integrator) daemon.Handler {
	return func(_ *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			ID string `json:"id"`
		}
		if perr := decode(params, &in); perr != nil {
			return nil, perr
		}
		out, err := it.Status(in.ID)
		if err != nil {
			if err == ErrNotFound {
				return nil, protocol.NewError(protocol.CodeNotFound, err.Error())
			}
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return outcome(out), nil
	}
}

func listHandler(it *Integrator) daemon.Handler {
	return func(_ *daemon.CallContext, _ json.RawMessage) (json.RawMessage, *protocol.Error) {
		recs, err := it.List()
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		out := make([]map[string]any, 0, len(recs))
		for _, r := range recs {
			out = append(out, map[string]any{
				"id": r.ID, "branch": r.Branch, "holder": r.Holder,
				"state": string(r.State), "base": r.Base, "merge_commit": r.MergeCommit,
			})
		}
		return result(map[string]any{"integrations": out}), nil
	}
}

// trunkHandler serves integrate.trunk — a READ-ONLY view of the authoritative
// trunk ref (the commit the integrator's CAS last advanced). The watch surface
// mirrors this directly instead of deriving the tip from integrate.list ordering
// (promote order is NOT created_at order, so "last promoted in the list" can be
// the superseded commit). Zero mutation.
func trunkHandler(it *Integrator) daemon.Handler {
	return func(_ *daemon.CallContext, _ json.RawMessage) (json.RawMessage, *protocol.Error) {
		tip, exists, err := it.TrunkTip()
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return result(map[string]any{
			"tip":    tip,
			"exists": exists,
			"branch": it.TrunkBranchName(),
		}), nil
	}
}

func outcome(o Outcome) json.RawMessage {
	return result(map[string]any{
		"id": o.ID, "state": string(o.State), "promoted": o.Promoted,
		"trunk_tip": o.TrunkTip, "reason": o.Reason, "retryable": o.Retryable,
	})
}

func decode(params json.RawMessage, v any) *protocol.Error {
	if len(params) == 0 {
		return protocol.NewError(protocol.CodeInvalidParams, "params required")
	}
	if err := json.Unmarshal(params, v); err != nil {
		return protocol.NewError(protocol.CodeInvalidParams, "invalid params")
	}
	return nil
}

func result(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
