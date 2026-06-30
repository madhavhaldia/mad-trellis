package manifest

import (
	"encoding/base64"
	"encoding/json"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

// RegisterMethods wires the classifier onto the daemon's JSON-RPC registry —
// the frozen external classify surface. Domain is "path" | "external" |
// "trunk"; any other value classifies upward (singular), keeping the function
// TOTAL over the wire too.
func RegisterMethods(reg *daemon.Registry, c *Classifier) error {
	for _, m := range []struct {
		name string
		h    daemon.Handler
	}{
		{"classify.classify", classifyHandler(c)},
		{"classify.route", routeHandler(c)},
	} {
		if err := reg.Register(m.name, m.h); err != nil {
			return err
		}
	}
	return nil
}

type classifyParams struct {
	Domain string `json:"domain"`
	Name   string `json:"name"`
}

func (p classifyParams) ref() ResourceRef {
	switch p.Domain {
	case "path":
		return ResourceRef{Domain: DomainPath, Name: p.Name}
	case "external":
		return ResourceRef{Domain: DomainExternal, Name: p.Name}
	case "trunk":
		return ResourceRef{Domain: DomainTrunk}
	default:
		return ResourceRef{Domain: Domain(-1), Name: p.Name} // unknown → classify-upward → singular
	}
}

func parse(params json.RawMessage) (classifyParams, *protocol.Error) {
	var in classifyParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &in); err != nil {
			return in, protocol.NewError(protocol.CodeInvalidParams, "invalid params")
		}
	}
	return in, nil
}

func classifyHandler(c *Classifier) daemon.Handler {
	return func(_ *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		in, perr := parse(params)
		if perr != nil {
			return nil, perr
		}
		return jsonResultM(map[string]any{"kind": c.Classify(in.ref()).String()}), nil
	}
}

func routeHandler(c *Classifier) daemon.Handler {
	return func(_ *daemon.CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		in, perr := parse(params)
		if perr != nil {
			return nil, perr
		}
		k, key := c.Route(in.ref())
		out := map[string]any{"kind": k.String(), "lease_key": nil}
		if key != nil {
			out["lease_key"] = base64.StdEncoding.EncodeToString(key)
		}
		return jsonResultM(out), nil
	}
}

func jsonResultM(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
