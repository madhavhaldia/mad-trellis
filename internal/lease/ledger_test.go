package lease

// Hand-authored invariant tests for project 2 (docs/0004 card 2) — the contract.
// Review-gated; negatives carry positive controls; NOT vibe-coded.

import (
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/daemon"
)

// --- helpers ----------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openLedger(t *testing.T, clk Clock) *Ledger {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"), clk)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func token() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "h-" + hex.EncodeToString(b[:])
}

// --- Inv 2(a): deterministic CAS mutual exclusion ---------------------------

func TestAcquireMutualExclusionRace(t *testing.T) {
	l := openLedger(t, nil)
	key := []byte("trunk")
	const n = 32
	var wg sync.WaitGroup
	granted := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := l.Acquire(key, token(), time.Hour)
			if err != nil {
				t.Errorf("acquire %d: %v", i, err)
				return
			}
			granted[i] = r.Granted
		}(i)
	}
	wg.Wait()
	count := 0
	for _, g := range granted {
		if g {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("exactly one acquirer must be granted under contention; got %d", count)
	}
}

func TestFailFastNoWait(t *testing.T) {
	l := openLedger(t, nil)
	key := []byte("k")
	if r, _ := l.Acquire(key, token(), time.Hour); !r.Granted {
		t.Fatal("first acquire must grant")
	}
	start := time.Now()
	r, err := l.Acquire(key, token(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if r.Granted {
		t.Fatal("a live lease must NOT be granted to a second acquirer")
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("acquire must fail-fast (no queue/wait); took %v", d)
	}
	if r.Holder == "" {
		t.Fatal("a conflict result should report the current holder")
	}
}

// --- Inv 3: TTL, renew-only-by-live-holder, durability ----------------------

func TestRenewOnlyByLiveHolder(t *testing.T) {
	clk := newFakeClock()
	l := openLedger(t, clk)
	key := []byte("k")
	hA, hB := token(), token()
	if r, _ := l.Acquire(key, hA, 10*time.Second); !r.Granted {
		t.Fatal("acquire A")
	}
	if r, _ := l.Renew(key, hA, 10*time.Second); !r.OK {
		t.Fatal("the live holder must renew")
	}
	if r, _ := l.Renew(key, hB, 10*time.Second); r.OK {
		t.Fatal("a non-holder must NOT renew")
	}
	clk.advance(time.Minute) // lapse past expiry
	if r, _ := l.Renew(key, hA, 10*time.Second); r.OK {
		t.Fatal("an expired lease must NOT be renewable")
	}
}

func TestDurabilityAcrossProcessDeath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	clk := newFakeClock()
	l1, err := Open(path, clk)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("k")
	h := token()
	r, _ := l1.Acquire(key, h, time.Hour)
	if !r.Granted {
		t.Fatal("acquire")
	}
	wantExp := r.ExpiresAt
	if err := l1.Close(); err != nil { // simulate process death (no release)
		t.Fatal(err)
	}
	l2, err := Open(path, clk) // reopen: a new "process"
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	info, ok, _ := l2.Inspect(key)
	if !ok || info.Holder != h {
		t.Fatalf("lease must survive process death; ok=%v holder=%q", ok, info.Holder)
	}
	if !info.ExpiresAt.Equal(wantExp) {
		t.Fatalf("expires_at must persist: want %v got %v", wantExp, info.ExpiresAt)
	}
	if r2, _ := l2.Acquire(key, token(), time.Hour); r2.Granted {
		t.Fatal("a still-live lease must not be acquirable after restart")
	}
}

// --- Inv 3-reclaim primitive: determinism, idempotency, no false-positive ---

func TestReclaimIfExpiredDeterminismIdempotency(t *testing.T) {
	clk := newFakeClock()
	l := openLedger(t, clk)
	key := []byte("k")
	h := token()
	l.Acquire(key, h, 10*time.Second)
	if r, _ := l.ReclaimIfExpired(key); r.Reclaimed {
		t.Fatal("must not reclaim a live lease")
	}
	clk.advance(11 * time.Second) // expired
	r1, _ := l.ReclaimIfExpired(key)
	if !r1.Reclaimed || r1.PriorHolder != h {
		t.Fatalf("an expired lease must reclaim with its prior holder; got %+v", r1)
	}
	if r2, _ := l.ReclaimIfExpired(key); r2.Reclaimed {
		t.Fatal("reclaim must be idempotent (no double-reclaim)")
	}
	if r, _ := l.Acquire(key, token(), time.Hour); !r.Granted {
		t.Fatal("a reclaimed lease must be re-acquirable")
	}
}

func TestReclaimNeverFreesLiveLease(t *testing.T) {
	clk := newFakeClock()
	l := openLedger(t, clk)
	key := []byte("k")
	h := token()
	l.Acquire(key, h, time.Hour)
	clk.advance(time.Minute) // still live (ttl 1h)
	if r, _ := l.ReclaimIfExpired(key); r.Reclaimed {
		t.Fatal("must NEVER free a non-expired lease (false-positive reclaim => two writers => corruption)")
	}
	if r, _ := l.Renew(key, h, time.Hour); !r.OK {
		t.Fatal("the live holder must still renew after a no-op reclaim")
	}
}

// --- Inv 3 fencing: a stale prior holder cannot re-act ----------------------

func TestFencingZombieHolder(t *testing.T) {
	clk := newFakeClock()
	l := openLedger(t, clk)
	key := []byte("k")
	hA := token()
	rA, _ := l.Acquire(key, hA, 10*time.Second)
	if !rA.Granted {
		t.Fatal("acquire A")
	}
	f0 := rA.Fence
	clk.advance(11 * time.Second)
	if rec, _ := l.ReclaimIfExpired(key); !rec.Reclaimed {
		t.Fatal("reclaim A after expiry")
	}
	hB := token()
	rB, _ := l.Acquire(key, hB, 10*time.Second)
	if !rB.Granted {
		t.Fatal("acquire B")
	}
	if rB.Fence <= f0 {
		t.Fatalf("fence must strictly increase across reclaim+reacquire: f0=%d fB=%d", f0, rB.Fence)
	}
	if r, _ := l.Renew(key, hA, time.Second); r.OK {
		t.Fatal("the zombie prior holder A must not renew")
	}
	if ok, _ := l.Release(key, hA); ok {
		t.Fatal("the zombie prior holder A must not release B's lease")
	}
	if info, _, _ := l.Inspect(key); info.Holder != hB {
		t.Fatalf("B must still hold the lease; got %q", info.Holder)
	}
}

// --- Inv 2(b): keys are opaque ----------------------------------------------

func TestOpaqueKey(t *testing.T) {
	l := openLedger(t, nil)
	k1 := []byte{0x00, 0xff, 0x01, 0x7f, 0x00}
	k2 := []byte("a/b:c\x00d") // looks structured but is just bytes
	h1, h2 := token(), token()
	if r, _ := l.Acquire(k1, h1, time.Hour); !r.Granted {
		t.Fatal("k1 acquire")
	}
	if r, _ := l.Acquire(k2, h2, time.Hour); !r.Granted {
		t.Fatal("k2 acquire (independent key)")
	}
	i1, ok1, _ := l.Inspect(k1)
	i2, ok2, _ := l.Inspect(k2)
	if !ok1 || !ok2 || i1.Holder != h1 || i2.Holder != h2 {
		t.Fatalf("opaque keys must be independent; got %q / %q", i1.Holder, i2.Holder)
	}
	// Positive control: no key shape is special-cased. A byte-identical key is
	// the SAME lease (held); a one-byte-different key is independent.
	k1same := []byte{0x00, 0xff, 0x01, 0x7f, 0x00}
	if r, _ := l.Acquire(k1same, token(), time.Hour); r.Granted {
		t.Fatal("a byte-identical key must be the same (held) lease, not a new grant")
	}
	k1diff := []byte{0x00, 0xff, 0x01, 0x7f, 0x01}
	if r, _ := l.Acquire(k1diff, token(), time.Hour); !r.Granted {
		t.Fatal("a one-byte-different key must be an independent lease")
	}
}

func TestReleaseIdempotent(t *testing.T) {
	l := openLedger(t, nil)
	key := []byte("k")
	h := token()
	l.Acquire(key, h, time.Hour)
	if ok, _ := l.Release(key, h); !ok {
		t.Fatal("the holder's release must succeed")
	}
	if ok, _ := l.Release(key, h); ok {
		t.Fatal("a second release must be a no-op (idempotent), not a re-success")
	}
	if r, _ := l.Acquire(key, token(), time.Hour); !r.Granted {
		t.Fatal("a released lease must be re-acquirable")
	}
}

// --- audit storage (lease->daemon edge), append-only ------------------------

func TestAuditSinkAppendOnly(t *testing.T) {
	l := openLedger(t, nil)
	sink := l.AuditSink()
	n0, _ := l.AuditCount()
	rec := daemon.AuditRecord{
		Timestamp:       time.Now(),
		Session:         "s-1",
		DecisionProject: "lease",
		DecisionKind:    "acquire",
		Payload:         []byte(`{"k":"v"}`),
	}
	if err := sink.Append(rec); err != nil {
		t.Fatal(err)
	}
	if err := sink.Append(rec); err != nil {
		t.Fatal(err)
	}
	if n1, _ := l.AuditCount(); n1 != n0+2 {
		t.Fatalf("audit must be append-only and accumulate; got %d want %d", n1, n0+2)
	}
}

// --- Wing 4: same-path contention (intent queue + fair hand-off, R8) ----------

// TestEnqueueDequeueQueue covers the basic intent-queue CAS: enqueue assigns FIFO
// positions, is idempotent per (key,session), dequeue removes, and the read-only
// Queue snapshot reflects holder + ordered live waiters.
func TestEnqueueDequeueQueue(t *testing.T) {
	l := openLedger(t, nil)
	key := []byte("trunk")

	// First waiter is head (position 0); a second is position 1.
	if r, err := l.Enqueue(key, "B", 0); err != nil || r.Position != 0 {
		t.Fatalf("first enqueue: pos=%d err=%v (want 0)", r.Position, err)
	}
	if r, err := l.Enqueue(key, "C", 0); err != nil || r.Position != 1 {
		t.Fatalf("second enqueue: pos=%d err=%v (want 1)", r.Position, err)
	}
	// Idempotent per (key,session): re-enqueuing B keeps its head position (0), it
	// does NOT go to the back of the queue.
	if r, err := l.Enqueue(key, "B", 0); err != nil || r.Position != 0 {
		t.Fatalf("re-enqueue B must keep head position: pos=%d err=%v (want 0)", r.Position, err)
	}

	snap, err := l.Queue(key)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if snap.Held {
		t.Fatalf("key has no holder yet but Queue reports held")
	}
	if len(snap.Waiters) != 2 || snap.Waiters[0].Session != "B" || snap.Waiters[1].Session != "C" {
		t.Fatalf("queue order must be [B,C]: %+v", snap.Waiters)
	}
	if snap.Waiters[0].Position != 0 || snap.Waiters[1].Position != 1 {
		t.Fatalf("queue positions must be 0,1: %+v", snap.Waiters)
	}

	// Dequeue B → C becomes head (position 0). Second dequeue of B is idempotent.
	if ok, err := l.Dequeue(key, "B"); err != nil || !ok {
		t.Fatalf("dequeue B: ok=%v err=%v", ok, err)
	}
	if ok, _ := l.Dequeue(key, "B"); ok {
		t.Fatalf("second dequeue of B must be a no-op (ok=false)")
	}
	snap, _ = l.Queue(key)
	if len(snap.Waiters) != 1 || snap.Waiters[0].Session != "C" || snap.Waiters[0].Position != 0 {
		t.Fatalf("after dequeue B, queue must be [C@0]: %+v", snap.Waiters)
	}
}

// TestWaiterTTLPrune proves a dead waiter's row expires by TTL with no liveness
// coupling: once the TTL elapses (injected clock), the waiter is pruned on the next
// queue/acquire read.
func TestWaiterTTLPrune(t *testing.T) {
	clk := newFakeClock()
	l := openLedger(t, clk)
	key := []byte("k")

	if _, err := l.Enqueue(key, "B", 50*time.Millisecond); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if snap, _ := l.Queue(key); len(snap.Waiters) != 1 {
		t.Fatalf("waiter must be live before TTL: %+v", snap.Waiters)
	}
	// Advance past the TTL → the dead waiter is pruned on the next read.
	clk.advance(200 * time.Millisecond)
	if snap, _ := l.Queue(key); len(snap.Waiters) != 0 {
		t.Fatalf("waiter must be pruned after TTL: %+v", snap.Waiters)
	}
}

// TestFairHandoffHeadWins is the core fairness contract: with a queue present, a
// FREE key is granted ONLY to the head; a non-head (or non-queued) caller is
// refused even though free. The CONTROL is the empty-queue path: the SAME acquire
// IS granted when no queue exists (so the refusal is the queue, not a regression).
func TestFairHandoffHeadWins(t *testing.T) {
	l := openLedger(t, nil)
	key := []byte("trunk")

	// CONTROL (non-vacuity): with NO queue, an acquire on the free key IS granted —
	// the unqueued common path is unchanged.
	if r, _ := l.Acquire(key, "X", time.Hour); !r.Granted {
		t.Fatalf("CONTROL: acquire on a free key with no queue must be granted")
	}
	if ok, _ := l.Release(key, "X"); !ok {
		t.Fatalf("release X: not ok")
	}

	// Queue B (head) then C (behind). Key is free.
	if r, _ := l.Enqueue(key, "B", 0); r.Position != 0 {
		t.Fatalf("B must be head")
	}
	if r, _ := l.Enqueue(key, "C", 0); r.Position != 1 {
		t.Fatalf("C must be position 1")
	}

	// A non-queued D is REFUSED on the free key (anti-barging), with B as head hint.
	if r, _ := l.Acquire(key, "D", time.Hour); r.Granted {
		t.Fatalf("anti-barging: non-queued D must be refused on the free key")
	} else if r.Holder != "" {
		t.Fatalf("D's refusal should be the queue (holder empty), got holder=%q", r.Holder)
	} else if r.Head != "B" {
		t.Fatalf("D's refusal hint should name head=B, got %q", r.Head)
	}

	// A queued NON-head (C) is also refused on the free key.
	if r, _ := l.Acquire(key, "C", time.Hour); r.Granted {
		t.Fatalf("non-head C must be refused on the free key")
	} else if r.Ahead != 1 {
		t.Fatalf("C is one behind the head; ahead=%d (want 1)", r.Ahead)
	}

	// The head B is granted and its waiter row is retired (queue drains to [C@0]).
	r, _ := l.Acquire(key, "B", time.Hour)
	if !r.Granted {
		t.Fatalf("head B must be granted the free key")
	}
	snap, _ := l.Queue(key)
	if !snap.Held || snap.Holder != "B" {
		t.Fatalf("after hand-off key must be held by B: %+v", snap)
	}
	if len(snap.Waiters) != 1 || snap.Waiters[0].Session != "C" {
		t.Fatalf("B's waiter row must be retired, leaving [C]: %+v", snap.Waiters)
	}
}

// TestExpiredHeadDoesNotBlockHandoff proves the queue is self-contained: when the
// head's TTL expires, it is pruned and the NEXT live waiter becomes head and wins
// the free key — a crashed head can never permanently starve the queue.
func TestExpiredHeadDoesNotBlockHandoff(t *testing.T) {
	clk := newFakeClock()
	l := openLedger(t, clk)
	key := []byte("k")

	if _, err := l.Enqueue(key, "B", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// C enqueues with a long TTL AFTER B, so B is head until B expires.
	clk.advance(10 * time.Millisecond)
	if _, err := l.Enqueue(key, "C", time.Hour); err != nil {
		t.Fatal(err)
	}
	// Expire B (but not C). C becomes the live head.
	clk.advance(100 * time.Millisecond)
	if r, _ := l.Acquire(key, "C", time.Hour); !r.Granted {
		t.Fatalf("after the head B expired, the next live waiter C must win the free key")
	}
}
