package substrate

import (
	"encoding/json"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

// RegisterMethods wires the substrate onto the daemon's JSON-RPC registry — the
// frozen external surface the launcher (project 5) codes against. The boundary
// is keyed off the caller's CONNECTION-BOUND session identity (Inv 4): the
// session is taken from cc.Session, never from params, so one client cannot
// provision or tear down another client's boundary. The handlers return an
// env-SPEC; APPLYING it (exec into cwd+env) is the launcher's job, kept OUT here.
func RegisterMethods(reg *daemon.Registry, s *Substrate) error {
	for _, m := range []struct {
		name string
		h    daemon.Handler
	}{
		{"substrate.provision", provisionHandler(s)},
		{"substrate.teardown", teardownHandler(s)},
	} {
		if err := reg.Register(m.name, m.h); err != nil {
			return err
		}
	}
	return nil
}

type provisionParams struct {
	Ports     int `json:"ports"`
	Resources []struct {
		Name   string `json:"name"`
		Domain string `json:"domain"`
		Ref    string `json:"ref"`
	} `json:"resources"`
}

func provisionHandler(s *Substrate) daemon.Handler {
	return func(cc *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in provisionParams
		if len(params) > 0 {
			if err := json.Unmarshal(params, &in); err != nil {
				return nil, protocol.NewError(protocol.CodeInvalidParams, "invalid params")
			}
		}
		req := Request{Ports: in.Ports}
		for _, r := range in.Resources {
			req.Resources = append(req.Resources, ResourceReq{Name: r.Name, Domain: r.Domain, Ref: r.Ref})
		}
		spec, err := s.Provision(string(cc.Session), req)
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		b, _ := json.Marshal(spec.Wire())
		return b, nil
	}
}

func teardownHandler(s *Substrate) daemon.Handler {
	return func(cc *daemon.CallContext, _ json.RawMessage) (json.RawMessage, *protocol.Error) {
		if err := s.Teardown(string(cc.Session)); err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error())
		}
		b, _ := json.Marshal(map[string]bool{"ok": true})
		return b, nil
	}
}
