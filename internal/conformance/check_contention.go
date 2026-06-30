package conformance

import (
	"fmt"

	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
)

// check_contention.go proves the Wing-4 SAME-PATH CONTENTION fairness clause (R8):
// when a convergent key frees with an intent queue behind it, the key is handed to
// the HEAD-of-queue waiter ONLY — a non-queued caller is REFUSED even though the
// key is free (anti-barging). This is the headless fair hand-off: the ledger gives
// FIFO order, the head wins the next acquire, waiters poll lease.queue for their
// turn (there is no push channel).
//
// BLACK BOX over the public lease RPC + the classifier-routed key only:
//
//	SETUP    — session A acquires the trunk lease (lease.acquire over the
//	           classify.route key); session B enqueues its intent (lease.enqueue,
//	           position 0 = head); A releases the lease.
//	NEGATIVE — with the key now FREE, a third NON-queued session C's acquire is
//	           REFUSED (granted=false, holder empty so the refusal is the QUEUE not
//	           a held lease), and lease.queue still names B at the head.
//	POSITIVE — the head-of-queue B then acquires the free key (granted=true) and the
//	           queue drains (B's waiter row retired) — so the refusal of C was the
//	           fair-hand-off rule, not a blanket denial.
//	CONTRAST — BEFORE any queue exists, a fresh session's acquire on the free key IS
//	           granted (the unqueued common path is unchanged), so the NEGATIVE is
//	           non-vacuous: the queue is what flips a free-key acquire to refused.
//
// CONTROL (non-vacuity): the fairness JUDGE must FLIP RED on a barge — feed it the
// violation (a non-queued caller granted ahead of the head) and assert it rejects;
// feed it the legitimate hand-off and assert it accepts. A judge that accepted a
// barge (or rejected everything) would make the Run verdict worthless.

func init() { RegisterCheck(contentionFairHandoff{}) }

type contentionFairHandoff struct{}

func (contentionFairHandoff) ID() string { return "contention-fair-handoff" }
func (contentionFairHandoff) OwnerProject() string {
	return "lease-ledger-mutex (Wing 4 — same-path contention)"
}
func (contentionFairHandoff) Clause() string {
	return "Wing-4 fairness (R8): a freed convergent key is granted ONLY to the head-of-queue waiter; a non-queued barger is refused even while free"
}

func (c contentionFairHandoff) Run(s *Scratch) Result {
	// The convergent key under contention — the classifier-routed trunk lease (the
	// ONLY legitimate source of a key; never fabricated).
	key, ok, err := s.RouteLeaseKey("trunk", "")
	if err != nil || !ok {
		return fail(c, "classify.route(trunk): ok=%v err=%v", ok, err)
	}

	// --- CONTRAST (self-non-vacuity): with NO queue, a fresh session's acquire on
	// the FREE key IS granted (the unqueued common path is unchanged). Release so the
	// real scenario starts from a free key.
	xConn, err := s.Dial()
	if err != nil {
		return fail(c, "X dial: %v", err)
	}
	defer xConn.Close()
	xg, _, _, err := acquire(xConn, key)
	if err != nil {
		return fail(c, "X acquire (no queue): %v", err)
	}
	if !xg {
		return fail(c, "CONTRAST FAILED: an acquire on a FREE key with NO queue was refused — the unqueued common path regressed")
	}
	if _, err := release(xConn, key); err != nil {
		return fail(c, "X release: %v", err)
	}

	// --- SETUP: A holds the lease; B enqueues intent; A releases.
	aConn, err := s.Dial()
	if err != nil {
		return fail(c, "A dial: %v", err)
	}
	defer aConn.Close()
	ag, _, _, err := acquire(aConn, key)
	if err != nil {
		return fail(c, "A acquire: %v", err)
	}
	if !ag {
		return fail(c, "A could not acquire the free key to model contention")
	}

	bConn, err := s.Dial()
	if err != nil {
		return fail(c, "B dial: %v", err)
	}
	defer bConn.Close()
	bSession, err := whoAmIOn(bConn)
	if err != nil {
		return fail(c, "B whoami: %v", err)
	}
	pos, err := enqueue(bConn, key)
	if err != nil {
		return fail(c, "B enqueue: %v", err)
	}
	if pos != 0 {
		return fail(c, "B enqueued behind an empty queue but got position %d (expected 0 = head)", pos)
	}

	if _, err := release(aConn, key); err != nil {
		return fail(c, "A release: %v", err)
	}

	// --- NEGATIVE: C is NOT queued. With the key free and B at the head, C's acquire
	// is REFUSED — and the refusal is the QUEUE (holder empty = free), naming B ahead.
	cConn, err := s.Dial()
	if err != nil {
		return fail(c, "C dial: %v", err)
	}
	defer cConn.Close()
	cGranted, cHolder, cHead, err := acquire(cConn, key)
	if err != nil {
		return fail(c, "C acquire (barge attempt): %v", err)
	}
	if cGranted {
		return fail(c, "ANTI-BARGING VIOLATED: a NON-queued session C was granted the free key ahead of the queued head B")
	}
	if cHolder != "" {
		return fail(c, "C's refusal was a HELD lease (holder=%s), not the queue — the key should have been free", cHolder)
	}
	if cHead != bSession {
		return fail(c, "C's refusal hint named head=%q, expected the queued head B=%q", cHead, bSession)
	}
	// lease.queue still names B at the head (C never displaced it).
	held, _, waiters, err := queueSnapshot(cConn, key)
	if err != nil {
		return fail(c, "queue snapshot after C barge: %v", err)
	}
	if held {
		return fail(c, "key reported HELD after C's refused barge — it should still be free")
	}
	if len(waiters) != 1 || waiters[0] != bSession {
		return fail(c, "queue should still be exactly [B=%s] after C's barge, got %v", bSession, waiters)
	}

	// --- POSITIVE: the head B acquires the free key (granted) and the queue drains.
	bGranted, _, _, err := acquire(bConn, key)
	if err != nil {
		return fail(c, "B (head) acquire: %v", err)
	}
	if !bGranted {
		return fail(c, "HEAD STARVED: the head-of-queue B was not granted the free key")
	}
	held, holder, waiters, err := queueSnapshot(bConn, key)
	if err != nil {
		return fail(c, "queue snapshot after B acquire: %v", err)
	}
	if !held || holder != bSession {
		return fail(c, "after the hand-off the key should be held by B=%s, got held=%v holder=%s", bSession, held, holder)
	}
	if len(waiters) != 0 {
		return fail(c, "the queue should have drained when the head took the key, got %v", waiters)
	}

	// The fairness judge must accept the REAL outcome (C refused, B granted).
	if !fairHandoff(cGranted, bGranted) {
		return fail(c, "the fairness judge rejected the genuine hand-off (cGranted=%v bGranted=%v)", cGranted, bGranted)
	}

	return pass(c, "free key handed to head-of-queue B only: non-queued C refused-while-free (holder empty, head=B), B granted, queue drained; "+
		"unqueued contrast grant confirms the queue is what refuses C")
}

func (c contentionFairHandoff) Control(s *Scratch) error {
	// Non-vacuity: the fairness JUDGE must FLIP RED on a barge — the exact violation
	// Run rules out (a non-queued caller granted the free key ahead of the head).
	if fairHandoff(true, false) {
		return fmt.Errorf("CONTROL VACUOUS: the fairness judge ACCEPTED a barge (non-queued caller granted, head refused) — it cannot catch anti-barging")
	}
	if fairHandoff(true, true) {
		return fmt.Errorf("CONTROL VACUOUS: the fairness judge ACCEPTED a barger being granted at all (double-grant / no ordering)")
	}
	// And it must ACCEPT the legitimate hand-off, so it is not rejecting everything.
	if !fairHandoff(false, true) {
		return fmt.Errorf("CONTROL VACUOUS: the fairness judge REJECTED the legitimate hand-off (barger refused, head granted) — it flags everything")
	}
	return nil
}

// fairHandoff is the fairness JUDGE: a hand-off is fair iff the non-queued barger
// was REFUSED on the free key AND the head-of-queue waiter was GRANTED it. Any
// outcome where the barger is granted is a fairness/anti-barging violation.
func fairHandoff(bargerGranted, headGranted bool) bool {
	return !bargerGranted && headGranted
}

// acquire drives lease.acquire over an existing connection, returning the grant,
// the reported holder (empty when free), and the head-of-queue hint.
func acquire(c *rpcclient.Client, key string) (granted bool, holder, head string, err error) {
	var out struct {
		Granted bool   `json:"granted"`
		Holder  string `json:"holder"`
		Head    string `json:"head"`
	}
	if cerr := c.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 120000}, &out); cerr != nil {
		return false, "", "", cerr
	}
	return out.Granted, out.Holder, out.Head, nil
}

func release(c *rpcclient.Client, key string) (bool, error) {
	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.Call("lease.release", map[string]any{"key": key}, &out); err != nil {
		return false, err
	}
	return out.OK, nil
}

func enqueue(c *rpcclient.Client, key string) (position int, err error) {
	var out struct {
		Position int `json:"position"`
	}
	if cerr := c.Call("lease.enqueue", map[string]any{"key": key}, &out); cerr != nil {
		return 0, cerr
	}
	return out.Position, nil
}

// queueSnapshot reads the public lease.queue snapshot: whether the key is held, its
// holder, and the ordered waiter sessions.
func queueSnapshot(c *rpcclient.Client, key string) (held bool, holder string, waiters []string, err error) {
	var out struct {
		Held    bool   `json:"held"`
		Holder  string `json:"holder"`
		Waiters []struct {
			Session string `json:"session"`
		} `json:"waiters"`
	}
	if cerr := c.Call("lease.queue", map[string]any{"key": key}, &out); cerr != nil {
		return false, "", nil, cerr
	}
	for _, w := range out.Waiters {
		waiters = append(waiters, w.Session)
	}
	return out.Held, out.Holder, waiters, nil
}
