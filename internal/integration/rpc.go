package integration

import (
	"encoding/json"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

// Register wires the integration plane onto the daemon's JSON-RPC registry. These
// integration.* methods are a deliberate, user-blessed ADDITION to the
// otherwise-frozen registry. Identity (Inv 4) is the connection-bound cc.Session
// on every MUTATING method (request/claim/verdict) — never a value from params:
// the requesting builder is the Holder, the claiming integrator is the Claimer.
// pending/status/list are read-only.
func Register(reg *daemon.Registry, ig *Integration) error {
	for _, m := range []struct {
		name string
		h    daemon.Handler
	}{
		{"integration.request", requestHandler(ig)},
		{"integration.pending", pendingHandler(ig)},
		{"integration.claim", claimHandler(ig)},
		{"integration.verdict", verdictHandler(ig)},
		{"integration.status", statusHandler(ig)},
		{"integration.cancel", cancelHandler(ig)},
		{"integration.list", listHandler(ig)},
	} {
		if err := reg.Register(m.name, m.h); err != nil {
			return err
		}
	}
	return nil
}

// integration.request — params {"branch": string (required, safe ref),
// "title": string?} -> {"id", "state": "requested"}. Holder = cc.Session.
func requestHandler(ig *Integration) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			Branch string `json:"branch"`
			Title  string `json:"title"`
		}
		if perr := decode(params, &in); perr != nil {
			return nil, perr
		}
		if in.Branch == "" {
			return nil, protocol.NewError(protocol.CodeInvalidParams, "branch required")
		}
		rec, err := ig.Request(string(cc.Session), in.Branch, in.Title)
		if err != nil {
			if err == ErrInvalidBranch {
				return nil, protocol.NewError(protocol.CodeInvalidParams, err.Error())
			}
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return result(map[string]any{"id": rec.ID(), "state": string(rec.State)}), nil
	}
}

// integration.pending — params {} -> {"pending": [{id,branch,title,state,
// created_at_ms}...]} listing `requested` records, oldest first. Read-only.
func pendingHandler(ig *Integration) daemon.Handler {
	return func(_ *daemon.CallContext, _ json.RawMessage) (json.RawMessage, *protocol.Error) {
		recs, err := ig.Pending()
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		out := make([]map[string]any, 0, len(recs))
		for _, r := range recs {
			out = append(out, map[string]any{
				"id":            r.ID(),
				"branch":        r.Branch,
				"title":         r.Title,
				"state":         string(r.State),
				"created_at_ms": r.CreatedAt.UnixMilli(),
			})
		}
		return result(map[string]any{"pending": out}), nil
	}
}

// integration.claim — params {"id"} (id == branch) -> {"ok","branch","title"}.
// CAS requested -> claimed, Claimer = cc.Session. ok=false (not an error) when
// the record isn't in `requested`.
func claimHandler(ig *Integration) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			ID string `json:"id"`
		}
		if perr := decode(params, &in); perr != nil {
			return nil, perr
		}
		if in.ID == "" {
			return nil, protocol.NewError(protocol.CodeInvalidParams, "id required")
		}
		ok, rec, err := ig.Claim(string(cc.Session), in.ID)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return result(map[string]any{"ok": ok, "branch": rec.Branch, "title": rec.Title}), nil
	}
}

// integration.verdict — params {"id","decision":"approve"|"reject",
// "feedback"?, "merge"?} -> {"ok","state"}. reject requires feedback; approve
// stores merge. ok=false when the CAS predicate (state == claimed) didn't hold.
func verdictHandler(ig *Integration) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			ID       string `json:"id"`
			Decision string `json:"decision"`
			Feedback string `json:"feedback"`
			Merge    string `json:"merge"`
		}
		if perr := decode(params, &in); perr != nil {
			return nil, perr
		}
		if in.ID == "" {
			return nil, protocol.NewError(protocol.CodeInvalidParams, "id required")
		}
		ok, state, err := ig.Verdict(string(cc.Session), in.ID, in.Decision, in.Feedback, in.Merge)
		if err != nil {
			if err == ErrFeedbackRequired || err == ErrBadDecision {
				return nil, protocol.NewError(protocol.CodeInvalidParams, err.Error())
			}
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return result(map[string]any{"ok": ok, "state": string(state)}), nil
	}
}

// integration.status — params {"branch"} -> {"found","id","state","feedback",
// "merge"}. The builder's poll. Read-only.
func statusHandler(ig *Integration) daemon.Handler {
	return func(_ *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			Branch string `json:"branch"`
		}
		if perr := decode(params, &in); perr != nil {
			return nil, perr
		}
		if in.Branch == "" {
			return nil, protocol.NewError(protocol.CodeInvalidParams, "branch required")
		}
		rec, found, err := ig.Status(in.Branch)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return result(map[string]any{
			"found":    found,
			"id":       rec.ID(),
			"state":    string(rec.State),
			"feedback": rec.Feedback,
			"merge":    rec.Merge,
		}), nil
	}
}

// integration.cancel — params {"id"} (id == branch) -> {"ok"}. CAS
// {requested|claimed} -> withdrawn, clearing a stale/abandoned request from the
// queue. ok=false (not an error) when the row is absent or already terminal. The
// holder is NOT read from params (Inv 4); cancel is gated by the state predicate.
func cancelHandler(ig *Integration) daemon.Handler {
	return func(_ *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			ID string `json:"id"`
		}
		if perr := decode(params, &in); perr != nil {
			return nil, perr
		}
		if in.ID == "" {
			return nil, protocol.NewError(protocol.CodeInvalidParams, "id required")
		}
		ok, err := ig.Cancel(in.ID)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return result(map[string]any{"ok": ok}), nil
	}
}

// integration.list — params {} -> {"records": [{id,branch,title,state,claimer,
// feedback,merge,created_at_ms,updated_at_ms}...]} listing ALL records in any
// state, newest-updated first. READ-ONLY (the watch surface's whole-queue view).
func listHandler(ig *Integration) daemon.Handler {
	return func(_ *daemon.CallContext, _ json.RawMessage) (json.RawMessage, *protocol.Error) {
		recs, err := ig.List()
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		out := make([]map[string]any, 0, len(recs))
		for _, r := range recs {
			out = append(out, map[string]any{
				"id":            r.ID(),
				"branch":        r.Branch,
				"title":         r.Title,
				"state":         string(r.State),
				"claimer":       r.Claimer,
				"feedback":      r.Feedback,
				"merge":         r.Merge,
				"created_at_ms": r.CreatedAt.UnixMilli(),
				"updated_at_ms": r.UpdatedAt.UnixMilli(),
			})
		}
		return result(map[string]any{"records": out}), nil
	}
}

func decode(params json.RawMessage, v any) *protocol.Error {
	if len(params) == 0 {
		return nil // all integration methods accept empty params (pending takes none)
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
