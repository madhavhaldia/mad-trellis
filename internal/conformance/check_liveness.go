package conformance

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// check_liveness.go proves the LIVENESS / FAILURE-RECOVERY clause (docs/0003
// §10a; Inv 3-reclaim, owned by liveness-recovery): no lock outlives its holder —
// the system makes progress after a single death. A holder dies mid-lease (its
// TTL lapses without renewal); the public liveness path (`mad-trellis recover`)
// RECLAIMS the expired lease so a fresh agent can acquire it; the trunk stays
// clean (a reclaim FREES a resource, it never mutates trunk).
//
// BLACK BOX over the public lease RPC + the `mad-trellis recover` CLI + observable
// state. It keys off the EXPLICIT TTL/lease state, NEVER a sleep-for-luck:
//
//   1. A "doomed holder" session acquires the trunk lease with a SHORT TTL and
//      then stops renewing (modeling death — the connection lingers, but a dead
//      agent does not heartbeat, so the lease must be reclaimed on TTL, not on the
//      socket closing).
//   2. The harness POLLS lease.inspect until held=false (the lease has lapsed past
//      its TTL — an EXPLICIT state, bounded by a deadline so a stuck daemon fails
//      fast rather than hanging).
//   3. `mad-trellis recover` reports reclaimed>=1 (the expired lease was reclaimed).
//   4. A FRESH session can now acquire the (reclaimed) trunk lease — progress was
//      made after the death.
//   5. The trunk ref is unchanged by recovery (reclaim frees, never promotes).
//
// CONTROL (non-vacuity): recovery must NOT reclaim a STILL-LIVE holder. The
// control holds the lease with a LONG TTL (alive), runs recover, and asserts the
// live lease is NOT reclaimed (held stays true, a rival is still denied) — so the
// reclaim in Run is genuinely TTL-death-gated, not a blanket "free everything."

func init() {
	RegisterCheck(livenessReclaim{})
	RegisterCheck(livenessAbortMidIntegration{})
}

type livenessReclaim struct{}

func (livenessReclaim) ID() string           { return "liveness-reclaim" }
func (livenessReclaim) OwnerProject() string { return "liveness-recovery" }
func (livenessReclaim) Clause() string {
	return "liveness: a dead holder's expired lease is reclaimed (recover), a fresh agent makes progress, trunk stays clean (Inv 3-reclaim)"
}

// shortTTL is the doomed holder's lease TTL — small so the harness reaches the
// expired state quickly, but the harness keys off lease.inspect held=false (the
// EXPLICIT lapsed state), never a fixed sleep.
const shortTTL = 150 * time.Millisecond

func (c livenessReclaim) Run(s *Scratch) Result {
	key, ok, err := s.RouteLeaseKey("trunk", "")
	if err != nil || !ok {
		return fail(c, "route trunk lease key: ok=%v err=%v", ok, err)
	}

	// 1) The doomed holder acquires the trunk lease with a SHORT TTL and then never
	// renews (death). We KEEP the connection open so the reclaim is driven by the TTL
	// lapse, not by the socket closing (a dead agent's lease must expire on TTL).
	doomed, err := s.Dial()
	if err != nil {
		return fail(c, "doomed dial: %v", err)
	}
	defer doomed.Close()
	var acq struct {
		Granted bool   `json:"granted"`
		Holder  string `json:"holder"`
	}
	if err := doomed.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": shortTTL.Milliseconds()}, &acq); err != nil {
		return fail(c, "doomed acquire: %v", err)
	}
	if !acq.Granted {
		return fail(c, "doomed holder could not acquire the free trunk lease")
	}

	// 2) Poll lease.inspect until the lease is no longer held (TTL lapsed) — the
	// EXPLICIT lapsed state, bounded by a deadline (no sleep-for-luck).
	if err := c.waitLeaseLapsed(s, key); err != nil {
		return fail(c, "%v", err)
	}

	// 3) `mad-trellis recover` reclaims the expired lease.
	rec := s.CLI("recover")
	if !rec.OK() {
		return fail(c, "recover failed: exit %d %s", rec.ExitCode, rec.Out())
	}
	if !recoverReclaimedAtLeastOne(rec.Out()) {
		return fail(c, "recover did not reclaim the expired lease (reclaimed=0); got: %s", trimLine(rec.Out()))
	}

	// 4) A FRESH session now acquires the reclaimed trunk lease — progress after death.
	fresh, err := s.Dial()
	if err != nil {
		return fail(c, "fresh dial: %v", err)
	}
	defer fresh.Close()
	var facq struct {
		Granted bool `json:"granted"`
	}
	if err := fresh.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 60000}, &facq); err != nil {
		return fail(c, "fresh acquire: %v", err)
	}
	if !facq.Granted {
		return fail(c, "NO PROGRESS: a fresh agent could not acquire the reclaimed trunk lease after the holder's death")
	}

	// 5) The trunk ref is untouched by recovery (a reclaim frees a lease; it never
	// promotes or mutates the trunk). No promote happened, so the trunk is unborn.
	if tip, _ := s.TrunkTip(); tip != "" {
		return fail(c, "recovery mutated the trunk (tip %s) — reclaim must free a lease, never advance trunk", short12(tip))
	}

	return pass(c, "doomed holder's lease lapsed (TTL %s); recover reclaimed it; a fresh agent acquired it; trunk untouched",
		shortTTL)
}

func (c livenessReclaim) Control(s *Scratch) error {
	// Non-vacuity: recovery must NOT reclaim a STILL-LIVE holder. A LIVE holder takes
	// the trunk lease with a LONG TTL; recover must report reclaimed=0 for it (a live
	// holder is never declared dead) AND a rival must still be denied (the lease is
	// genuinely still held). If recover reclaimed a live lease, the Run's reclaim
	// would be a meaningless "free everything," not a TTL-death gate.
	key, ok, err := s.RouteLeaseKey("trunk", "")
	if err != nil || !ok {
		return fmt.Errorf("control route key: ok=%v err=%v", ok, err)
	}
	live, err := s.Dial()
	if err != nil {
		return fmt.Errorf("control live dial: %w", err)
	}
	defer live.Close()
	var acq struct {
		Granted bool `json:"granted"`
	}
	if err := live.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 120000}, &acq); err != nil {
		return fmt.Errorf("control live acquire: %w", err)
	}
	if !acq.Granted {
		return fmt.Errorf("control setup: live holder could not acquire the lease")
	}

	// Run recover immediately (the lease is far from its TTL → still live).
	rec := s.CLI("recover")
	if !rec.OK() {
		return fmt.Errorf("control recover failed: exit %d %s", rec.ExitCode, rec.Out())
	}
	if recoverReclaimedAtLeastOne(rec.Out()) {
		return fmt.Errorf("CONTROL VACUOUS: recover reclaimed a STILL-LIVE holder's lease (%s) — reclaim is not TTL-death-gated, so the Run reclaim proves nothing", trimLine(rec.Out()))
	}

	// And the live lease is genuinely still held: a rival acquire is denied.
	rival, err := s.Dial()
	if err != nil {
		return fmt.Errorf("control rival dial: %w", err)
	}
	defer rival.Close()
	var racq struct {
		Granted bool `json:"granted"`
	}
	if err := rival.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 60000}, &racq); err != nil {
		return fmt.Errorf("control rival acquire: %w", err)
	}
	if racq.Granted {
		return fmt.Errorf("CONTROL VACUOUS: a rival acquired the lease that was supposed to be still-live-held — the live state is not real")
	}
	return nil
}

// waitLeaseLapsed polls lease.inspect until held=false (the lease lapsed past its
// TTL) or a bounded deadline elapses. It keys off the EXPLICIT lapsed state, not a
// fixed sleep — so a slow daemon fails fast with a clear error rather than a flake.
func (c livenessReclaim) waitLeaseLapsed(s *Scratch, key string) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cl, err := s.Dial()
		if err != nil {
			return fmt.Errorf("inspect dial: %w", err)
		}
		var info struct {
			Exists bool `json:"exists"`
			Held   bool `json:"held"`
		}
		ierr := cl.Call("lease.inspect", map[string]any{"key": key}, &info)
		cl.Close()
		if ierr != nil {
			return fmt.Errorf("lease.inspect: %w", ierr)
		}
		if info.Exists && !info.Held {
			return nil // lapsed: present row, past TTL → reclaimable
		}
		time.Sleep(15 * time.Millisecond)
	}
	return fmt.Errorf("lease did not lapse past its %s TTL within the poll deadline", shortTTL)
}

// recoverReclaimedAtLeastOne parses the `mad-trellis recover` line
// "recovery: reclaimed=N aborted=N torn_down=N dead=[...]" and reports reclaimed>=1.
func recoverReclaimedAtLeastOne(out string) bool {
	n, ok := parseRecoverField(out, "reclaimed=")
	return ok && n >= 1
}

// parseRecoverField extracts the integer after a "field=" token in the recover
// output (e.g. "reclaimed=" → 2).
func parseRecoverField(out, field string) (int, bool) {
	i := strings.Index(out, field)
	if i < 0 {
		return 0, false
	}
	rest := out[i+len(field):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:j])
	if err != nil {
		return 0, false
	}
	return n, true
}

// trimLine returns the first non-empty line of a multi-line output, for compact
// failure detail.
func trimLine(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

// ----------------------------------------------------------------------------
// liveness-abort-mid-integration — the mid-INTEGRATION death half of the liveness
// clause (fix #5; docs/0003 §10a "kill mid-lease AND mid-integration"; acceptance
// [6/7] "kill mid-integration -> trunk byte-identical, abort fired"). The reclaim
// check above covers mid-LEASE death; this covers a holder dying mid-integration.
//
// MECHANISM (read against internal/liveness/recover.go + internal/integrator):
// liveness aborts an in-flight integration when it is `validating` AND its holder
// holds NO live lease (recover.go step 3: `in.State == "validating" && !live[holder]`).
// The integrator reaches `validating` BEFORE acquiring the trunk lease (Promote:
// setValidating then leases.Acquire), so a promote BLOCKED at the lease step (a
// rival holds the trunk lease) parks the integration durably in `validating`.
//
// DETERMINISTIC BLACK-BOX SCENARIO:
//  1. Establish a born trunk (so the integration validates against a real base).
//  2. A `doomed` holder submits a branch OVER A WIRE CONNECTION (so the holder is
//     that connection's session) and holds NO lease.
//  3. A `rival` session holds the trunk lease — blocking the doomed promote.
//  4. The doomed promote reaches `validating` then refuses at the lease step
//     (retryable). The integration is parked in `validating`; its holder holds no
//     live lease (only the rival does).
//  5. Poll integrate.status until `validating` (EXPLICIT state, no sleep).
//  6. `mad-trellis recover` reports aborted>=1; integrate.status becomes `aborted`;
//     TrunkTip is byte-identical to the pre-recover tip (the base — abort never
//     moves a ref).
//
// CONTROL (non-vacuity): a LIVE in-flight holder must NOT be aborted. The control
// makes the holder live (the SUBMIT connection itself holds the trunk lease, so
// its own promote is blocked from re-acquiring its OWN live lease → parked in
// `validating` with a LIVE holder). recover must report aborted=0 and leave the
// integration in-flight — proving the abort in Run is genuinely death-gated.

type livenessAbortMidIntegration struct{}

func (livenessAbortMidIntegration) ID() string { return "liveness-abort-mid-integration" }
func (livenessAbortMidIntegration) OwnerProject() string {
	return "liveness-recovery + integrator-trunk"
}
func (livenessAbortMidIntegration) Clause() string {
	return "liveness: a dead mid-integration holder's in-flight (validating) integration is ABORTED by recover, trunk byte-identical (Inv 3-reclaim / acceptance 6)"
}

func (c livenessAbortMidIntegration) Run(s *Scratch) Result {
	agent, err := s.NewAgent("midint")
	if err != nil {
		return fail(c, "new agent: %v", err)
	}
	base, err := s.EstablishTrunkBase(agent, map[string]string{"a.txt": "base\n"})
	if err != nil {
		return fail(c, "establish trunk base: %v", err)
	}

	// The doomed holder authors a feature and submits it OVER A WIRE CONNECTION (so
	// the integration holder is that connection's session — and it holds NO lease).
	if err := agent.Checkout("midint-wb", "origin/trunk"); err != nil {
		return fail(c, "checkout: %v", err)
	}
	if _, err := agent.Commit("midint feature", map[string]string{"b.txt": "added\n"}); err != nil {
		return fail(c, "commit: %v", err)
	}
	ref, err := agent.PushBranch("midint")
	if err != nil {
		return fail(c, "push: %v", err)
	}
	doomed, err := s.Dial()
	if err != nil {
		return fail(c, "doomed dial: %v", err)
	}
	defer doomed.Close()
	id, err := s.SubmitOn(doomed, ref)
	if err != nil {
		return fail(c, "doomed submit: %v", err)
	}

	// A RIVAL session holds the trunk lease, blocking the doomed promote at the lease
	// step (the key comes ONLY from classify.route).
	key, ok, err := s.RouteLeaseKey("trunk", "")
	if err != nil || !ok {
		return fail(c, "route trunk key: ok=%v err=%v", ok, err)
	}
	rival, err := s.Dial()
	if err != nil {
		return fail(c, "rival dial: %v", err)
	}
	defer rival.Close()
	var racq struct {
		Granted bool `json:"granted"`
	}
	if err := rival.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 120000}, &racq); err != nil {
		return fail(c, "rival acquire: %v", err)
	}
	if !racq.Granted {
		return fail(c, "rival could not hold the trunk lease (setup failed)")
	}

	// The doomed promote reaches `validating` then refuses at the held lease.
	state, retryable, err := s.PromoteOn(doomed, id)
	if err != nil {
		return fail(c, "doomed promote: %v", err)
	}
	if state != "validating" || !retryable {
		return fail(c, "the lease-blocked promote should park the integration in validating+retryable; got state=%q retryable=%v", state, retryable)
	}

	// Poll integrate.status until validating (EXPLICIT in-flight state, no sleep).
	if err := c.waitState(s, id, "validating"); err != nil {
		return fail(c, "%v", err)
	}

	// The doomed holder holds no live lease (only the rival does). recover must ABORT
	// the in-flight integration. Capture the pre-recover trunk tip for the
	// byte-identical assertion.
	//
	// NOTE on the daemon's periodic recovery loop (5s): it would EVENTUALLY abort
	// this same integration. Our on-demand `recover` runs sub-second after reaching
	// validating, so it virtually always wins the race and reports aborted>=1; but to
	// stay DETERMINISTIC we accept either "our CLI reported the abort" OR "the
	// integration was already aborted by the governed liveness path" — in both cases
	// the mid-integration abort FIRED (the safety property), and we still assert the
	// terminal aborted state + byte-identical trunk below.
	preStatus, _ := s.IntegrationStatus(id)
	preTip, _ := s.TrunkTip()
	rec := s.CLI("recover")
	if !rec.OK() {
		return fail(c, "recover failed: exit %d %s", rec.ExitCode, rec.Out())
	}
	aborted, _ := parseRecoverField(rec.Out(), "aborted=")
	if aborted < 1 && preStatus != "aborted" {
		return fail(c, "recover did not abort the dead mid-integration (aborted=%d, pre-status=%q); got: %s", aborted, preStatus, trimLine(rec.Out()))
	}

	// The integration's status is aborted.
	if st, err := s.IntegrationStatus(id); err != nil {
		return fail(c, "status after recover: %v", err)
	} else if st != "aborted" {
		return fail(c, "the in-flight integration must be aborted after recover; got state=%q", st)
	}

	// Trunk is byte-identical to the pre-recover tip (abort frees, never promotes).
	postTip, _ := s.TrunkTip()
	if postTip != preTip {
		return fail(c, "BREACH: recover moved the trunk %s -> %s aborting a mid-integration (must be byte-identical)", short12(preTip), short12(postTip))
	}
	if postTip != base {
		return fail(c, "the trunk advanced past base %s -> %s though no promote landed", short12(base), short12(postTip))
	}

	return pass(c, "doomed mid-integration holder parked at validating; recover aborted=%d; integration aborted; trunk byte-identical at %s",
		aborted, short12(postTip))
}

func (c livenessAbortMidIntegration) Control(s *Scratch) error {
	// Non-vacuity: a LIVE in-flight holder must NOT be aborted. We make the holder
	// LIVE by having the SUBMIT connection itself hold the trunk lease — so its own
	// promote is refused at the re-acquire of its OWN live lease, parking the
	// integration in `validating` with a genuinely LIVE holder. recover must report
	// aborted=0 and leave it in-flight; if recover aborted a live holder, the Run's
	// abort would be a blanket "free everything," not a death gate.
	agent, err := s.NewAgent("midint-ctl")
	if err != nil {
		return fmt.Errorf("control new agent: %w", err)
	}
	if _, err := s.EstablishTrunkBase(agent, map[string]string{"a.txt": "base\n"}); err != nil {
		return fmt.Errorf("control establish trunk: %w", err)
	}
	if err := agent.Checkout("midint-ctl-wb", "origin/trunk"); err != nil {
		return fmt.Errorf("control checkout: %w", err)
	}
	if _, err := agent.Commit("midint-ctl feature", map[string]string{"b.txt": "added\n"}); err != nil {
		return fmt.Errorf("control commit: %w", err)
	}
	ref, err := agent.PushBranch("midint-ctl")
	if err != nil {
		return fmt.Errorf("control push: %w", err)
	}

	// The holder connection submits AND holds the trunk lease (long TTL) → it is LIVE.
	holder, err := s.Dial()
	if err != nil {
		return fmt.Errorf("control holder dial: %w", err)
	}
	defer holder.Close()
	key, ok, err := s.RouteLeaseKey("trunk", "")
	if err != nil || !ok {
		return fmt.Errorf("control route key: ok=%v err=%v", ok, err)
	}
	var hacq struct {
		Granted bool `json:"granted"`
	}
	if err := holder.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 120000}, &hacq); err != nil {
		return fmt.Errorf("control holder acquire: %w", err)
	}
	if !hacq.Granted {
		return fmt.Errorf("control holder could not acquire the trunk lease")
	}
	id, err := s.SubmitOn(holder, ref)
	if err != nil {
		return fmt.Errorf("control submit: %w", err)
	}
	// The holder's own promote: setValidating then re-acquire its OWN live lease →
	// refused (the CAS excludes a live holder), so it parks in validating.
	state, _, err := s.PromoteOn(holder, id)
	if err != nil {
		return fmt.Errorf("control promote: %w", err)
	}
	if state != "validating" {
		return fmt.Errorf("control: expected the holder's promote to park at validating; got %q", state)
	}
	if err := c.waitState(s, id, "validating"); err != nil {
		return fmt.Errorf("control %v", err)
	}

	// recover must NOT abort the LIVE holder's in-flight integration.
	rec := s.CLI("recover")
	if !rec.OK() {
		return fmt.Errorf("control recover failed: exit %d %s", rec.ExitCode, rec.Out())
	}
	if aborted, ok := parseRecoverField(rec.Out(), "aborted="); ok && aborted > 0 {
		return fmt.Errorf("CONTROL VACUOUS: recover aborted a LIVE in-flight holder (aborted=%d) — the abort is not death-gated, so the Run abort proves nothing", aborted)
	}
	if st, err := s.IntegrationStatus(id); err != nil {
		return fmt.Errorf("control status: %w", err)
	} else if st != "validating" {
		return fmt.Errorf("CONTROL VACUOUS: the LIVE holder's integration left `validating` (now %q) without being killed", st)
	}
	return nil
}

// waitState polls integrate.status until it reaches want (the EXPLICIT in-flight
// state) or a bounded deadline elapses — keyed off explicit state, never a sleep.
func (c livenessAbortMidIntegration) waitState(s *Scratch, id, want string) error {
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		st, err := s.IntegrationStatus(id)
		if err != nil {
			return fmt.Errorf("poll integrate.status: %w", err)
		}
		last = st
		if st == want {
			return nil
		}
		time.Sleep(15 * time.Millisecond)
	}
	return fmt.Errorf("integration %s did not reach %q within the poll deadline (last=%q)", id, want, last)
}
