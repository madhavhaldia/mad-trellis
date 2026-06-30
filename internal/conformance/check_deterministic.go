package conformance

import (
	"fmt"
)

// check_deterministic.go proves the JOINT-2b NO-LLM-IN-LOCK-PATH invariant
// (docs/0003 §10a "the no-LLM-anywhere-in-lock-path (2b) check"; Inv 2(b), a
// JOINT clause across lease-ledger-mutex + manifest-classifier +
// daemon-arbiter-protocol — conformance-checked here). The exclusive-access path
// is classify.route (decide WHICH key) -> lease.acquire (the CAS lock decision).
// It must be DETERMINISTIC: identical inputs give byte-identical lock decisions
// across N repetitions, with NO probabilistic / LLM component anywhere.
//
// BLACK BOX over the public classify.route + lease RPC only:
//   - classify.route(trunk) returns the SAME base64 lease key on every call (the
//     classification is a pure function of its inputs, not a sampled/inferred one).
//   - the lock DECISION is deterministic: with the trunk lease FREE, acquire is
//     granted; with it HELD by another session, a rival acquire is denied — and
//     this verdict is identical across N repetitions (never "sometimes granted").
//     A probabilistic lock would flicker.
//
// CONTROL (non-vacuity): a non-deterministic decision MUST flip the check RED. The
// control runs an explicitly NON-deterministic oracle (a coin flip seeded off wall
// time) through the SAME determinism judge the Run uses and asserts the judge
// reports it as NON-deterministic — proving the judge would catch a probabilistic
// lock decision (it is not vacuously calling everything deterministic).

func init() { RegisterCheck(deterministicLockPath{}) }

type deterministicLockPath struct{}

func (deterministicLockPath) ID() string { return "deterministic-lock-path" }
func (deterministicLockPath) OwnerProject() string {
	return "lease-ledger-mutex + manifest-classifier + daemon-arbiter-protocol (joint 2b)"
}
func (deterministicLockPath) Clause() string {
	return "joint-2b: no LLM/probabilistic component in the exclusive-access path; identical inputs give byte-identical lock decisions (Inv 2b)"
}

// lockReps is how many times the lock path is replayed; a probabilistic component
// would diverge across this many identical trials.
const lockReps = 16

func (c deterministicLockPath) Run(s *Scratch) Result {
	// --- (1) classify.route is a pure function: the SAME key every time.
	keys := make([]string, 0, lockReps)
	for i := 0; i < lockReps; i++ {
		key, ok, err := s.RouteLeaseKey("trunk", "")
		if err != nil || !ok {
			return fail(c, "classify.route(trunk) rep %d: ok=%v err=%v", i, ok, err)
		}
		keys = append(keys, key)
	}
	if !allEqual(keys) {
		return fail(c, "NON-DETERMINISTIC ROUTE: classify.route(trunk) returned different keys across %d reps: %v", lockReps, distinct(keys))
	}
	key := keys[0]

	// --- (2) the lock DECISION is deterministic. With the lease FREE, acquire is
	// granted every time (acquire+release each rep to reset to the free state). With
	// the lease HELD by a rival, a fresh session's acquire is denied every time.
	freeDecisions := make([]bool, 0, lockReps)
	for i := 0; i < lockReps; i++ {
		granted, err := c.acquireOnFreshSession(s, key)
		if err != nil {
			return fail(c, "free-state acquire rep %d: %v", i, err)
		}
		freeDecisions = append(freeDecisions, granted)
	}
	if !allTrue(freeDecisions) {
		return fail(c, "NON-DETERMINISTIC LOCK: acquire on a FREE lease was not granted on every rep: %v", freeDecisions)
	}

	// Hold the lease from a stable holder session, then replay a rival acquire.
	holder, err := s.Dial()
	if err != nil {
		return fail(c, "holder dial: %v", err)
	}
	defer holder.Close()
	var hacq struct {
		Granted bool `json:"granted"`
	}
	if err := holder.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 120000}, &hacq); err != nil {
		return fail(c, "holder acquire: %v", err)
	}
	if !hacq.Granted {
		return fail(c, "could not hold the trunk lease to model contention")
	}
	heldDecisions := make([]bool, 0, lockReps)
	for i := 0; i < lockReps; i++ {
		granted, err := c.acquireOnFreshSession(s, key)
		if err != nil {
			return fail(c, "held-state acquire rep %d: %v", i, err)
		}
		heldDecisions = append(heldDecisions, granted)
	}
	if anyTrue(heldDecisions) {
		return fail(c, "NON-DETERMINISTIC LOCK: a rival acquire on a HELD lease was sometimes granted (fail-open / probabilistic): %v", heldDecisions)
	}

	// The determinism judge must pass the real lock-decision streams.
	if nd := nonDeterministic(freeDecisions); nd {
		return fail(c, "the free-state decision stream judged non-deterministic (should be all-grant)")
	}
	if nd := nonDeterministic(heldDecisions); nd {
		return fail(c, "the held-state decision stream judged non-deterministic (should be all-deny)")
	}

	// --- (3) CROSS-PROJECT 2b (fix #11): 10a is the named owner of the joint 2b across
	// lease-ledger-mutex + manifest-classifier + daemon-arbiter. Drive the daemon's
	// dispatch/authz/routing path (classify.route — the WHICH-key decision that gates
	// exclusive access, the project-1 daemon-arbiter slice composed with the project-3
	// classifier) with IDENTICAL inputs and assert the response ENVELOPES are
	// BYTE-IDENTICAL across N reps. A probabilistic/LLM component anywhere on this path
	// would make the verdict bytes diverge.
	if r := c.checkByteIdenticalVerdicts(s); !r.Pass {
		return r
	}

	return pass(c, "classify.route(trunk) is a pure function (one key over %d reps); lock decision deterministic: %d×grant free, %d×deny held; "+
		"dispatch/authz/routing verdict envelopes byte-identical across reps. STATIC no-LLM-reachability is discharged by the 1/2/3 hand-authored slices (no model/sampling import on the lock path).",
		lockReps, lockReps, lockReps)
}

// checkByteIdenticalVerdicts is the joint-2b cross-project probe: it replays the
// daemon's dispatch/authz/routing path (classify.route over a fixed (domain,name)
// AND a convergent-path verdict) with identical inputs and asserts the raw response
// envelopes are byte-identical across reps — proving the verdict is a pure function,
// rolling in project-1's daemon-arbiter slice + project-3's classifier.
func (c deterministicLockPath) checkByteIdenticalVerdicts(s *Scratch) Result {
	type probe struct{ domain, name string }
	probes := []probe{
		{"trunk", ""},                   // the convergent trunk → a routed lease key
		{"path", "migrations/0001.sql"}, // a classifier verdict over a path
		{"external", "prod-database"},   // an external-resource verdict
	}
	for _, p := range probes {
		first := ""
		for i := 0; i < lockReps; i++ {
			raw, err := s.CallRaw("classify.route", map[string]any{"domain": p.domain, "name": p.name})
			if err != nil {
				return fail(c, "classify.route(%s,%q) rep %d: %v", p.domain, p.name, i, err)
			}
			if i == 0 {
				first = raw
				continue
			}
			if raw != first {
				return fail(c, "NON-DETERMINISTIC VERDICT: classify.route(%s,%q) returned DIFFERENT envelopes across identical inputs (rep %d: %q vs %q) — a probabilistic component is in the dispatch/authz/routing path",
					p.domain, p.name, i, raw, first)
			}
		}
	}
	return pass(c, "byte-identical verdicts")
}

// acquireOnFreshSession dials a NEW session (a distinct agent identity), attempts
// to acquire key, and — if granted — releases it so the next rep sees the same
// free state. The granted verdict is what the determinism judge inspects.
func (c deterministicLockPath) acquireOnFreshSession(s *Scratch, key string) (bool, error) {
	cl, err := s.Dial()
	if err != nil {
		return false, err
	}
	defer cl.Close()
	var acq struct {
		Granted bool `json:"granted"`
	}
	if err := cl.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 120000}, &acq); err != nil {
		return false, err
	}
	if acq.Granted {
		var rel struct {
			OK bool `json:"ok"`
		}
		if err := cl.Call("lease.release", map[string]any{"key": key}, &rel); err != nil {
			return acq.Granted, err
		}
	}
	return acq.Granted, nil
}

func (c deterministicLockPath) Control(s *Scratch) error {
	// Non-vacuity: the determinism judge must DETECT a probabilistic decision. Run an
	// explicitly NON-deterministic oracle (a coin flip off a changing seed) through
	// the SAME nonDeterministic judge and confirm it is flagged. If a coin flip is
	// judged "deterministic", the judge is broken and the Run's determinism verdict
	// is worthless.
	coin := flipStream(lockReps)
	if !nonDeterministic(coin) {
		return fmt.Errorf("CONTROL VACUOUS: a deliberately probabilistic (coin-flip) decision stream %v was judged DETERMINISTIC — the judge cannot catch an LLM/random lock decision", coin)
	}
	// And the judge must NOT flag a genuinely constant stream (so it is not flagging
	// everything). A constant stream models a deterministic lock decision.
	constant := make([]bool, lockReps) // all-false
	if nonDeterministic(constant) {
		return fmt.Errorf("CONTROL VACUOUS: a constant decision stream was judged non-deterministic — the judge flags everything, so its deterministic verdict means nothing")
	}
	return nil
}

// nonDeterministic is the determinism JUDGE: a sequence of identical-input lock
// decisions is DETERMINISTIC iff every element is equal (the decision is a pure
// function of its inputs). Any variation across identical trials is the signature
// of a probabilistic / sampled / LLM component in the lock path.
func nonDeterministic(decisions []bool) bool {
	return !allBoolEqual(decisions)
}

// flipStream produces a VARYING boolean stream — a control input modeling a
// non-deterministic (LLM/random) lock oracle for the determinism judge. It is a
// FIXED ALTERNATING pattern, NOT a wall-time coin flip: the ONLY property the judge
// needs is "the stream VARIES across identical trials" (any variation is the
// signature of a probabilistic component), and a deterministic alternating pattern
// supplies exactly that variation without importing math/rand or wall-time
// nondeterminism into the test (the harness itself stays pure + reproducible). It is
// guaranteed non-constant for n>=2.
func flipStream(n int) []bool {
	out := make([]bool, n)
	for i := 0; i < n; i++ {
		out[i] = i%2 == 0
	}
	return out
}

func allEqual(ss []string) bool {
	for i := 1; i < len(ss); i++ {
		if ss[i] != ss[0] {
			return false
		}
	}
	return true
}

func allTrue(bs []bool) bool {
	for _, b := range bs {
		if !b {
			return false
		}
	}
	return true
}

func anyTrue(bs []bool) bool {
	for _, b := range bs {
		if b {
			return true
		}
	}
	return false
}

func allBoolEqual(bs []bool) bool {
	for i := 1; i < len(bs); i++ {
		if bs[i] != bs[0] {
			return false
		}
	}
	return true
}

func distinct(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
