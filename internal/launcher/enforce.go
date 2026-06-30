package launcher

import (
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrBlocked is returned by EnforceLeaseHeld when a governed action is denied —
// the session does not (provably) hold the required lease. It is the mechanical
// boundary refusal (Inv 4): the action does not proceed.
var ErrBlocked = errors.New("blocked: required lease not held by this session")

// EnforceLeaseHeld is the mechanical boundary-enforcement POINT (Inv 4): a
// reusable, deterministic gate that permits a governed action ONLY when
// `session` provably holds `key` in the ledger RIGHT NOW. The integrator
// (project 6), the singular-gate (project 7), and the cooperative adapter (9b)
// all route their governed actions through a check of this shape; the launcher
// owns the primitive.
//
// Two invariants are baked in:
//
//   - No probabilistic component anywhere on this path (the launcher's local
//     2(b) slice): the verdict is a pure function of the ledger's answer.
//   - FAIL CLOSED (Inv 9, the soul): every uncertainty is a DENY, never an
//     allow. A daemon error, a malformed reply, a missing/expired lease, or a
//     lease held by a DIFFERENT session all return ErrBlocked. The only path to
//     "allowed" is an explicit, current, same-session hold.
//
// An UNCOOPERATIVE agent (one that calls no adapter and was granted no lease)
// therefore cannot pass this gate for any governed key — there is no lease in
// its name, so every check denies. nil = allowed; non-nil = blocked.
func EnforceLeaseHeld(c Conn, session string, key []byte) error {
	if c == nil || session == "" || len(key) == 0 {
		return fmt.Errorf("%w: incomplete enforcement context", ErrBlocked)
	}
	var info struct {
		Exists bool   `json:"exists"`
		Holder string `json:"holder"`
		Held   bool   `json:"held"`
	}
	if err := c.Call("lease.inspect", map[string]any{"key": base64.StdEncoding.EncodeToString(key)}, &info); err != nil {
		// Uncertainty about ledger state is a DENY, never a pass.
		return fmt.Errorf("%w: ledger unreachable (%v)", ErrBlocked, err)
	}
	if !info.Exists || !info.Held || info.Holder != session {
		return ErrBlocked
	}
	return nil
}
