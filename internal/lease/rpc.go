package lease

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/daemon"
	"github.com/madhavhaldia/mad-trellis/internal/protocol"
)

// RegisterMethods wires the lease ledger onto the daemon's JSON-RPC registry —
// the frozen external lease surface (out-of-process clients: the launcher and
// adapter). The lease HOLDER is bound to the caller's connection session
// (Inv 4): callers act as themselves; the holder is never taken from params, so
// one session cannot release/renew another's lease. Keys are opaque, base64 on
// the wire. (reclaim_if_expired is intentionally NOT exposed — it is the
// in-process liveness primitive, not a client method.)
func RegisterMethods(reg *daemon.Registry, l *Ledger) error {
	for _, m := range []struct {
		name string
		h    daemon.Handler
	}{
		{"lease.acquire", acquireHandler(l)},
		{"lease.renew", renewHandler(l)},
		{"lease.release", releaseHandler(l)},
		{"lease.inspect", inspectHandler(l)},
		{"lease.list", listHandler(l)},
		// Wing 4 (same-path contention, R8) — the three user-blessed additions to the
		// otherwise-frozen lease surface: a fair intent queue + FIFO hand-off so a
		// waiter on a held convergent/path key gets ORDER + anti-barging, headless
		// (turn discovered by polling lease.queue).
		{"lease.enqueue", enqueueHandler(l)},
		{"lease.dequeue", dequeueHandler(l)},
		{"lease.queue", queueHandler(l)},
	} {
		if err := reg.Register(m.name, m.h); err != nil {
			return err
		}
	}
	return nil
}

func acquireHandler(l *Ledger) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			Key   string `json:"key"`
			TTLMs int64  `json:"ttl_ms"`
		}
		if perr := decodeParams(params, &in); perr != nil {
			return nil, perr
		}
		key, perr := decodeKey(in.Key)
		if perr != nil {
			return nil, perr
		}
		if in.TTLMs <= 0 {
			return nil, protocol.NewError(protocol.CodeInvalidParams, "ttl_ms must be > 0")
		}
		res, err := l.Acquire(key, string(cc.Session), time.Duration(in.TTLMs)*time.Millisecond)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return jsonResult(map[string]any{
			"granted":       res.Granted,
			"holder":        res.Holder,
			"expires_at_ms": res.ExpiresAt.UnixMilli(),
			"fence":         res.Fence,
			// Wing-4 advisory hints (additive — existing clients ignore them): on a
			// refusal caused by the fair queue, "head" names the session that WILL win
			// next and "ahead" is how many live waiters are ahead of this caller.
			"head":  res.Head,
			"ahead": res.Ahead,
		}), nil
	}
}

// enqueueHandler registers the caller's intent to acquire a held key (Wing 4). The
// waiter identity is the connection-bound session (Inv 4), never taken from params.
func enqueueHandler(l *Ledger) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			Key   string `json:"key"`
			TTLMs int64  `json:"ttl_ms"`
		}
		if perr := decodeParams(params, &in); perr != nil {
			return nil, perr
		}
		key, perr := decodeKey(in.Key)
		if perr != nil {
			return nil, perr
		}
		var ttl time.Duration // 0 → ledger applies DefaultWaiterTTL
		if in.TTLMs > 0 {
			ttl = time.Duration(in.TTLMs) * time.Millisecond
		}
		res, err := l.Enqueue(key, string(cc.Session), ttl)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return jsonResult(map[string]any{"position": res.Position, "ahead": res.Ahead}), nil
	}
}

// dequeueHandler withdraws the caller's intent for a key (Wing 4). Identity is the
// connection session, so one session can never withdraw another's intent.
func dequeueHandler(l *Ledger) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			Key string `json:"key"`
		}
		if perr := decodeParams(params, &in); perr != nil {
			return nil, perr
		}
		key, perr := decodeKey(in.Key)
		if perr != nil {
			return nil, perr
		}
		ok, err := l.Dequeue(key, string(cc.Session))
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return jsonResult(map[string]any{"ok": ok}), nil
	}
}

// queueHandler returns the read-only contention snapshot for a key (Wing 4): the
// live holder and the ordered live waiters. Read-only — it never mutates state.
func queueHandler(l *Ledger) daemon.Handler {
	return func(_ *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			Key string `json:"key"`
		}
		if perr := decodeParams(params, &in); perr != nil {
			return nil, perr
		}
		key, perr := decodeKey(in.Key)
		if perr != nil {
			return nil, perr
		}
		snap, err := l.Queue(key)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		waiters := make([]map[string]any, 0, len(snap.Waiters))
		for _, w := range snap.Waiters {
			waiters = append(waiters, map[string]any{"session": w.Session, "position": w.Position})
		}
		return jsonResult(map[string]any{
			"held":    snap.Held,
			"holder":  snap.Holder,
			"waiters": waiters,
		}), nil
	}
}

func renewHandler(l *Ledger) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			Key   string `json:"key"`
			TTLMs int64  `json:"ttl_ms"`
		}
		if perr := decodeParams(params, &in); perr != nil {
			return nil, perr
		}
		key, perr := decodeKey(in.Key)
		if perr != nil {
			return nil, perr
		}
		if in.TTLMs <= 0 {
			return nil, protocol.NewError(protocol.CodeInvalidParams, "ttl_ms must be > 0")
		}
		res, err := l.Renew(key, string(cc.Session), time.Duration(in.TTLMs)*time.Millisecond)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return jsonResult(map[string]any{"ok": res.OK, "expires_at_ms": res.ExpiresAt.UnixMilli()}), nil
	}
}

func releaseHandler(l *Ledger) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			Key string `json:"key"`
		}
		if perr := decodeParams(params, &in); perr != nil {
			return nil, perr
		}
		key, perr := decodeKey(in.Key)
		if perr != nil {
			return nil, perr
		}
		ok, err := l.Release(key, string(cc.Session))
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return jsonResult(map[string]any{"ok": ok}), nil
	}
}

func inspectHandler(l *Ledger) daemon.Handler {
	return func(_ *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			Key string `json:"key"`
		}
		if perr := decodeParams(params, &in); perr != nil {
			return nil, perr
		}
		key, perr := decodeKey(in.Key)
		if perr != nil {
			return nil, perr
		}
		info, exists, err := l.Inspect(key)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		return jsonResult(map[string]any{
			"exists":        exists,
			"holder":        info.Holder,
			"expires_at_ms": info.ExpiresAt.UnixMilli(),
			"fence":         info.Fence,
			"held":          info.Held,
		}), nil
	}
}

func listHandler(l *Ledger) daemon.Handler {
	return func(_ *daemon.CallContext, _ json.RawMessage) (json.RawMessage, *protocol.Error) {
		holders, err := l.ListHolders()
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		out := make([]map[string]any, 0, len(holders))
		for _, h := range holders {
			out = append(out, map[string]any{
				"key":           base64.StdEncoding.EncodeToString(h.Key),
				"holder":        h.Holder,
				"expires_at_ms": h.ExpiresAt.UnixMilli(),
				"fence":         h.Fence,
			})
		}
		return jsonResult(map[string]any{"holders": out}), nil
	}
}

func decodeKey(s string) ([]byte, *protocol.Error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeInvalidParams, "key must be base64")
	}
	if len(b) == 0 {
		return nil, protocol.NewError(protocol.CodeInvalidParams, "key required")
	}
	return b, nil
}

func decodeParams(params json.RawMessage, v any) *protocol.Error {
	if len(params) == 0 {
		return protocol.NewError(protocol.CodeInvalidParams, "params required")
	}
	if err := json.Unmarshal(params, v); err != nil {
		return protocol.NewError(protocol.CodeInvalidParams, "invalid params")
	}
	return nil
}

func jsonResult(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
