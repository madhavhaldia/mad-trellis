package liveness

import (
	"encoding/json"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

// RegisterMethods exposes liveness.scan — an on-demand recovery pass (the same
// idempotent Scan the periodic loop runs). It lets a test / the conformance
// harness / an operator trigger recovery deterministically without waiting for
// the ticker. It mutates nothing directly — it only invokes the ledger/integrator
// triggers — so exposing it cannot weaken the no-mutate contract.
func RegisterMethods(reg *daemon.Registry, r *Recoverer) error {
	return reg.Register("liveness.scan", func(_ *daemon.CallContext, _ json.RawMessage) (json.RawMessage, *protocol.Error) {
		rep, err := r.Scan()
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		b, _ := json.Marshal(map[string]any{
			"reclaimed":    rep.Reclaimed,
			"aborted":      rep.Aborted,
			"torn_down":    rep.TornDown,
			"dead_holders": rep.DeadHolders,
		})
		return b, nil
	})
}
