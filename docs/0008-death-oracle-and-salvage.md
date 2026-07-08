# 0008 — The death oracle, quarantine, and salvage-before-destroy

> Status: PROPOSED 2026-07-06. Redesign of the liveness/recovery plane after the
> 2026-07-05 incident: a macOS clamshell sleep + dark-wake caused the daemon to
> declare a live session dead and run `git worktree remove --force` on its
> boundary, destroying uncommitted work. This document replaces the single-signal,
> single-phase, destructive reclaim with (1) a suspension-aware lease clock,
> (2) connection reality as a veto on destruction, (3) resurrectable sessions, and
> (4) a two-phase reclaim in which authority is revoked promptly but state is
> destroyed lazily — and never before it is salvaged into a git ref.

## Incident summary (what this fixes)

On 2026-07-05 the host slept at 17:39:21 and dark-woke at 17:56:37. Five seconds
later the daemon's liveness scan reclaimed the session-liveness lease
(`mad-trellis:session:v1:s-3-…`), classified the session as dead, and tore down its
boundary via `git worktree remove --force`. The launcher, agent, and MCP server were
all still alive; the worktree held uncommitted work that is now unrecoverable. No
integration ran; the daemon did not restart; the branch was intact at its base
commit. The proximate mechanism was correct code doing exactly what it was designed
to do — the design was wrong.

## Problem — three compounding defects

**D1 — The death oracle is a wall-clock TTL.** Lease expiry is a wall-clock
timestamp (`lease.SystemClock` → `time.Now()`; `expires_at` nanos in SQLite,
`internal/lease/ledger.go`). The session TTL is 30s
(`internal/launcher/launcher.go:45`), renewed every 15s. Host suspension advances
the wall clock while *no process on the machine can possibly renew*. Any sleep
longer than 30s therefore manufactures a "failed to renew" signal out of nothing.
The TTL conflates "30 seconds of running time passed without a renewal" (a real
death signal) with "30 seconds of wall time passed" (which sleep produces for free).

**D2 — Death is a one-way ratchet, unappealable even by a live holder.** Once the
session lease expires: renew fails (`ledger.go` renew CAS requires
`expires_at > now` — only a live holder may renew, Inv 3), `session.attach` fails
(it requires the liveness lease to be *currently held*, `internal/session/session.go`),
and the token store then prunes the dead session's capability token
(`pruneDeadLocked`). So after any suspension > TTL the session is irrecoverably
dead *by construction*, no matter who wakes first, even though the launcher still
holds an unforgeable capability token proving it is the same principal, and even
though the worktree is still intact on disk. Ledger belief and process reality
diverge with no reconciliation path.

**D3 — The response to a death verdict is irreversible destruction.**
`liveness.Scan` → `substrate.Teardown` → `worktree.Remove` →
`git worktree remove --force` (`internal/worktree/worktree.go:154`). The branch
survives, but uncommitted and untracked work is destroyed forever. The same
destructive remove also runs on the launcher's clean-exit path
(`internal/launcher/launcher.go:190`), so even a *true* clean exit discards any
work the agent had not committed.

The failure needed all three: D1 produced a false verdict, D2 made the verdict
unappealable, D3 made it fatal. Each is fixed independently; together they are
defense in depth.

## Invariant analysis — what Inv 3 actually requires

Inv 3 says *no lock outlives its holder; the system always makes progress after any
single agent dies*. That obligates **prompt lease reclaim** — a dead holder's trunk
lease, singular grants, and port reservations must be freed so other agents
progress.

It does **not** obligate boundary destruction. A dead session's worktree is
forkable state (Inv 1): private to that agent, invisible to every other agent
except through the integration plane. It contends for nothing except disk. Leaving
it on disk for days violates no clause of the safety property (§ "The safety
property", GROUNDING.md) — no agent can observe it, no convergent resource is
blocked by it, no singular resource is affected by it.

Eager destruction therefore buys **zero safety** and carries the maximum possible
false-positive cost: destroying a live agent's work is exactly the corruption the
mission sentence forbids ("however you drive your agents, the road can't kill
you"). Under Inv 9's asymmetry, the current design has the polarity backwards:
it classifies *downward* (treats a maybe-dead session's state as safely
destroyable) when uncertainty demands classifying *upward* (preserve).

**The design rule that falls out: death revokes authority; it never destroys
work.** Authority revocation (leases, grants, ports, fencing) is prompt and
TTL-driven. State destruction is lazy, explicitly gated, and preceded by salvage.

## Design

### Part 1 — Suspension-aware lease clock (fixes D1)

The daemon cannot receive macOS sleep/wake notifications without IOKit (cgo —
forbidden). It does not need them. Suspension is detectable from inside pure Go as
a **clock discontinuity**: the recovery loop already ticks every 5s
(`internal/app/compose.go:264`); a suspended daemon's ticks simply do not run.

Mechanism:

- The daemon keeps a heartbeat stamp `lastTick` (wall-clock, updated every scan
  tick; also persisted best-effort to the ledger each tick so the detector
  survives a daemon restart that follows a sleep).
- At the top of each `Scan`: `gap := now − lastTick`. If `gap > suspicionGap`
  (default 15s — three missed ticks), the daemon knows that *it* was not running
  during the gap, and therefore that no verdict formed across the gap is sound:
  holders on the same host were almost certainly also suspended and could not
  renew.
- On a detected discontinuity, before reading expired leases, the scan performs a
  single **expiry rebase**: every *held* lease whose `expires_at` falls inside
  `(lastTick, now + rebaseSlack]` is extended to `now + its TTL` (the ledger grows
  a `ttl` column so the rebase knows each lease's own horizon; the write is one
  conditional UPDATE — the CAS discipline is unchanged). Holders that are actually
  alive renew within TTL/2 of wake and continue normally; holders that truly died
  during the sleep simply expire again one TTL later. The cost of a false
  *negative* here is a one-TTL delay in reclaiming a genuinely dead session —
  seconds of lost parallelism, the acceptable direction under Inv 9.
- The rebase is deterministic and mechanical (a clock fact, not an inference —
  Inv 2 is untouched: lock *acquisition* remains a pure CAS; the rebase only ever
  extends a held lease, which is strictly conservative).

This preserves per-key lease semantics for contended resources: a holder suspended
mid-promote still loses the trunk lease *one TTL after wake* if it does not renew,
so other agents progress (Inv 3), and the existing fence bump prevents its stale
writes. Only the manufactured instant-expiry-at-wake is eliminated.

### Part 2 — Connection reality as a veto on destruction (hardens the oracle)

The daemon already owns connection-bound identity over a Unix socket
(`internal/daemon/identity.go`); the launcher holds one persistent connection for
the life of the session, and Unix-socket connections survive host sleep (they die
only with a process or a daemon restart). This is a *direct kernel fact* about
process reality, strictly stronger than any TTL.

Mechanism: the daemon tracks, per session identity, the set of currently-open
connections bound to it (it already binds identity at accept; this adds a
counter decremented on close). The liveness scan's **teardown/quarantine step**
(not the per-key lease reclaim) adds one clause: a session with any open bound
connection is ALIVE — skip it, audit `liveness.skipped_connected_holder`.

Scope discipline, so this cannot rot into a liveness backdoor:

- Connection state **never extends a lease** and never influences who holds a
  contended lock (Inv 2). A wedged-but-connected launcher still loses its trunk
  lease on TTL; others progress. The connection veto guards exactly one thing:
  the transition of *this session's own boundary* into the condemned state.
- After a daemon restart there are no connections; the existing token re-attach
  path (`keepalive.go`) restores them within one renew tick. The Part 1 rebase
  (with the persisted `lastTick`) covers the sleep+restart combination.
- EOF becomes a *fast true-death hint*: when a session's last bound connection
  closes without a clean teardown, the scan may check that session eagerly instead
  of waiting a full TTL. (Optimization, not a correctness requirement — the TTL
  remains the backstop for launcher-SIGKILL/power-loss where EOF may be the only
  signal anyway.)

### Part 3 — Resurrectable sessions (fixes D2)

The capability token is an unforgeable, durable proof of principal identity that
already survives daemon restarts. There is no reason a *lease expiry* should
invalidate it while the boundary still exists.

Widened `session.attach` semantics (same wire signature — behavior change to an
existing frozen method, to be reviewed as a contract change per the registry
discipline):

1. Token resolves, session lease **live and held by that session** → rebind
   (today's behavior).
2. Token resolves, session lease **expired or reclaimed**, boundary **not yet
   garbage-collected** → *resurrect*: atomically re-acquire the session-liveness
   lease for the original session id (a normal acquire CAS on the session key —
   only one claimant can win; the fence has already been bumped by any reclaim, so
   anything holding pre-death fenced state is invalidated), flip a condemned
   boundary back to live, rebind the connection. Audit `session.resurrected`.
3. Token resolves, boundary **gone** (GC ran) → attach fails **permanently and
   loudly** with a distinct error carrying the salvage ref name (Part 4). The
   launcher surfaces it and exits rather than running the agent ungoverned; the
   MCP layer keeps its existing fail-soft advisory behavior.

Token pruning (`pruneDeadLocked`) re-keys from "session lease not live" to
"session boundary garbage-collected (or token cold for N days)" — a token must
outlive any state its holder could still legitimately reclaim.

The launcher keepalive's recovery loop (`recoverSession`) needs no structural
change: its existing re-attach path now succeeds in case 2, so a session that
slept through any number of TTLs self-heals on wake with zero user-visible
ceremony — Inv 13's ambient-governance experience, restored.

### Part 4 — Two-phase reclaim: quarantine, salvage, lazy GC (fixes D3)

`substrate.Teardown` splits into two operations with different clocks:

**Phase A — Quarantine (prompt, on a confirmed death signal).** Everything Inv 3
actually needs, nothing it doesn't:

- leases reclaimed (unchanged — per-key CAS, fence bump),
- ports released, singular grants revoked,
- the boundary marked `condemned` in durable substrate state
  (timestamp + reason: `liveness-reclaim` | `clean-exit-abandon`),
- for the container grain: the container is **stopped** (compute revoked — that is
  the enforcement boundary for an uncooperative agent) but its filesystem/worktree
  is preserved.

A condemned boundary accepts no governed operations (its leases are gone and its
fence is stale — already mechanically true), is skipped by provisioning (the slug
stays occupied), and is listed by the read-only watch surface so the state is
visible without adding any interaction loop (Inv 13).

**Phase B — GC (lazy, salvaging, refusal-biased).** Runs only when a condemned
boundary is older than the cold window (default **72h**), or on explicit
`mad-trellis gc` / `mad-trellis session rm <id>`. Ordered, and fails toward
preservation:

1. **Salvage.** If the worktree is dirty or has untracked files, snapshot it
   without touching the user-visible branch or index: temp `GIT_INDEX_FILE` →
   `git add -A` → `git write-tree` → `git commit-tree` (parent = current worktree
   HEAD; message records session id, reason, condemned-at/GC-at timestamps) →
   ref written at `refs/mad-trellis/salvage/<session>/<unix-ts>` in the canonical
   repo, so it survives the worktree's removal. A clean tree records
   `salvage.skipped_clean`.
2. **Verify.** The salvage ref exists and its tree hash matches a re-read of the
   worktree state captured in step 1. Any mismatch or error → **GC refuses**, the
   boundary stays condemned, the failure is audited, and a later pass retries.
   `git worktree remove --force` is never reachable except through a verified
   salvage (or a verifiably clean tree).
3. **Destroy.** Remove the worktree, prune the admin entry, delete per-agent
   state dirs, drop the capability token. The branch `nm/<session>` is left
   intact, as today.

The same salvage step is hoisted into the **clean-exit teardown**
(`launcher.go` teardown defer) and `ReclaimOrphan`: every path that can reach
`worktree.Remove` salvages first. Clean exits with uncommitted work — a real user
loss today, incident or no incident — become recoverable. Salvage refs older than
a retention window (default 30 days) are pruned by `mad-trellis gc`.

Resurrection (Part 3, case 2) and GC race on the condemned state; the substrate's
existing reserve/in-flight slug guard (`substrate.go` `reserving` set) serializes
them, and the condemned→live flip is a CAS on the durable state row, so exactly
one of {resurrect, destroy} wins.

## What deliberately does not change

- **Per-key TTL semantics for contended leases.** The trunk lease, singular
  grants, and waiter queues keep strict expire-and-reclaim behavior; fencing
  remains the stale-writer guard. Progress after a true death is still bounded by
  one TTL (Inv 3).
- **The lock path stays a pure CAS** — no connection state, no PID checks, no
  probabilistic anything in "who holds exclusive access" (Inv 2). Process-reality
  signals only ever *veto destruction* or *permit resurrection of the same
  principal via an unforgeable token*.
- **Liveness stays detector + trigger.** The rebase is a ledger primitive it
  invokes; salvage/GC are substrate/worktree mechanisms it invokes. Ownership
  boundaries from docs/0003 are unchanged.
- **No new interaction surface.** Condemned boundaries and salvage refs appear in
  watch/status (read-only); recovery is automatic via attach. The user never
  operates mad-trellis to survive a sleep (Inv 13).

## Conformance clauses (each with a non-vacuous control)

1. **Sleep does not kill.** Using the injected `lease.Clock`, jump wall time past
   several TTLs with the launcher's connection open and no renewals during the
   gap; scan; the boundary must survive and the session must renew/resurrect.
   *Control:* disable the discontinuity rebase and the connection veto → the same
   scenario must go red (boundary condemned).
2. **True death still reclaims.** Kill the launcher (no connection, no token
   re-attach), let the TTL lapse with no clock gap; leases must be reclaimed
   within one scan and the boundary condemned. *Control:* a still-renewing holder
   in the same scenario must not be touched.
3. **Salvage precedes destruction.** Condemn a boundary with dirty + untracked
   files; force GC; the salvage ref must exist and contain exactly the dirty
   state; only then is the worktree gone. *Control:* inject a salvage failure
   (unwritable ref namespace) → GC must refuse and the worktree must remain.
4. **Resurrection is exclusive and fenced.** Two concurrent attaches with the same
   token against an expired session: exactly one wins the re-acquire CAS; a write
   fenced at the pre-death fence is rejected. *Control:* drop the fence check →
   red.
5. **GC-after-resurrect cannot destroy a live boundary.** Resurrect, then run a
   (stale) GC pass for the same slug: the slug guard/CAS must make GC a no-op.
   *Control:* bypass the guard → red.

## Rejected alternatives

- **Longer TTL (e.g., 10 minutes).** Shrinks the false-positive window; eliminates
  nothing (an overnight sleep still kills), and slows true-death reclaim by the
  same factor. Tuning, not design.
- **IOKit sleep/wake notifications.** Correct signal, wrong cost: cgo is
  non-negotiable-forbidden for the shipped binary, and it is macOS-only. The tick
  gap detector is pure Go, cross-platform, and covers "daemon paused" causes
  beyond sleep (SIGSTOP, VM freeze, scheduler starvation).
- **PID registration + `kill(pid, 0)` as the reality check.** Weaker than the
  connection the daemon already holds (PID reuse, no restart story), and adds a
  client-asserted fact where a kernel-verified one exists.
- **Never tearing down at all.** Leaks ports, container compute, and disk without
  bound, and leaves an uncooperative container running ungoverned — Phase A
  quarantine is genuinely needed promptly; only *destruction* is not.
- **Salvage into the session branch (`git add -A && git commit` on `nm/<id>`).**
  Mutates a branch the user/integrator may be inspecting and conflates "what the
  agent committed" with "what the reaper found on disk". A ref under
  `refs/mad-trellis/salvage/` keeps provenance clean.

## Rollout

1. **Salvage-before-destroy on every remove path** (Part 4 step 1–2 hoisted into
   the current single-phase teardown). Smallest diff, converts any remaining
   false positive from corruption into inconvenience. Land first with clause 3.
2. **Discontinuity rebase** (Part 1) + `ttl` column + persisted `lastTick`, with
   clause 1's control.
3. **Quarantine/GC split** (Part 4 proper) + condemned state in watch.
4. **Widened attach / resurrection** (Part 3) + token-prune re-keying — the
   contract-sensitive step; registry review per the frozen-registry rule.
5. **Connection veto + EOF hint** (Part 2).

Steps 1–2 alone would have fully prevented the 2026-07-05 incident; steps 3–5
remove the class.

## Footnote — version skew

The incident daemon was an older binary than the installed
`~/.local/bin/mad-trellis` (stale inode). Orthogonal to this design, but cheap to
close: `mad-trellis doctor` should compare the running daemon's `main.version`
/`main.commit` (already stamped via `-ldflags -X`, and available over the
contract) against the invoking CLI's, and warn on mismatch; `make install` should
suggest a daemon restart when the socket is live.
