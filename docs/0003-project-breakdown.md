# 0003 — Project Breakdown (the "What")

*Status: accepted. Companion to [GROUNDING.md](../GROUNDING.md) (the invariants), [0001](./0001-form-and-architecture.md) (form), [0002](./0002-stack.md) (stack).*
*This doc is the WHAT: mad-substrate decomposed into projects, each with a crisp responsibility, that together sum to the whole. It is the anchor — every implementation task traces to a project here.*

*Produced by an 8-agent deliberation (4 diverse decompositions → 3 adversarial critiques → synthesis), then hardened by a 6-agent adversarial verification pass that found 4 high + 8 medium defects (4 false alarms filtered). All confirmed defects are resolved below. A final 3-agent confirmation pass verified every fix closed with no new seams introduced — enshrineable, zero must-fix.*

---

## How to read this
- Each **project** owns one or more **invariant clauses** and a slice of capability. Multi-claim invariants are split into **named clauses** so every clause has **exactly one owner** (no orphans, no silent double-ownership).
- A project's **what** is its mission; **in/out of scope** keep boundaries clean; **owns** lists the invariant clauses it is accountable for; **depends-on** is build-order.
- Layer-2 features (planning, quality review, parallelization tuning) are **not** projects — only their seams are cut. See GROUNDING "Scope & Layering".

## Invariant-clause ownership map (the backbone)
Every one of the 13 invariants has exactly one accountable owner per clause:

| Inv | Clause | Owner |
|----|--------|-------|
| 1 | isolation of forkable **FS + runtime + ports + local-state** | isolation-substrate |
| 2 | (a) atomic CAS lock path | lease-ledger-mutex |
| 2 | (b) **no LLM/probabilistic component anywhere in the exclusive-access path** | **joint**: lease-ledger-mutex + manifest-classifier + daemon-arbiter-protocol — conformance-checked |
| 3 | 3-durable: store, TTL write, renew-only-by-live-holder, **reclaim-if-expired evaluation** | lease-ledger-mutex |
| 3 | 3-reclaim: dead-holder **detection + trigger only** (never mutates ledger/trunk) | liveness-recovery |
| 4 | enforce at boundary + **shim-install fail-closed**; "sandbox-harder" tail discharged by grain dial | session-launcher-shim (tail: isolation-substrate) |
| 5 | 5-arbiter: single **process + ledger** arbiter | daemon-arbiter-protocol |
| 5 | 5-integrator: single trunk promoter | integrator-trunk |
| 5 | "star never mesh / no peer-to-peer" | conjunction of 5-arbiter + 5-integrator + Inv-1 runtime isolation — conformance-checked |
| 6 | nothing merges silently / validated gate | integrator-trunk (single-writer half discharged by the lease) |
| 7 | trunk advances only via validated integration; no agent reaches origin | integrator-trunk |
| 8 | default-deny singular | singular-gate |
| 9 | classify upward | manifest-classifier |
| 10 | 10-decoupling: couple to no agent/host/orchestrator; MCP out of daemon API | daemon-arbiter-protocol |
| 10 | 10-grainswap: worktree→container→VM dial | isolation-substrate |
| 11 | coupling is declaration, never modification | manifest-classifier |
| 12 | load-bearing closed loop: intake side / output side | session-launcher-shim + integrator-trunk; **GUARANTEE** conformance-checked |
| 12 | read-only surface of the loop | watch-view-surface |
| 13 | 13-interaction + **no-goals/no-task-dispatch** negative obligation | session-launcher-shim |
| 13 | 13-readonly: only new surface is read-only | watch-view-surface |

> The cooperative **host-adapter owns NO Layer-1 invariant** — safety never depends on it (GROUNDING Inv. 4).

---

## The 12 projects (by phase)

### P0 — Spine root
**1. daemon-arbiter-protocol** — *Daemon, Arbiter & Protocol Spine*
- **What:** the single long-lived headless Go daemon that is THE arbiter (a **process singleton**, not only a ledger singleton), plus the frozen local Unix-socket JSON-RPC contract every client codes against.
- **In:** single-daemon process lifecycle (start-if-absent on first CLI call, single-instance socket lock / stale-socket handling, supervised restart); JSON-RPC method registry + error taxonomy + version field, frozen first as a stub; socket authz / unspoofable session identity; the **decision-audit write-interface** all decision projects emit through (so the lease store never becomes a coupling hub); **operational/mechanism diagnostics** (daemon/launcher/integrator failure reasons).
- **Out:** lease semantics/storage; integration/trunk; ledger-state recovery (liveness-recovery); any governance policy; the agent-facing MCP dialect (kept OUT — that lives in the adapter).
- **Owns:** 5-arbiter, 10-decoupling; joint 2(b).
- **Depends-on:** —

### P1 — Coordination core (parallel; both depend only on the spine)
**2. lease-ledger-mutex** — *Lease Ledger & Deterministic Mutual Exclusion*
- **What:** the embedded-SQLite lease store and the deterministic, no-LLM lock service — the single source of truth for who holds exclusive access.
- **In:** atomic CAS acquire/renew/release over **opaque** keys (zero probabilistic components, ever); TTL with **renew-only-by-live-holder** authorization; a deterministic **`reclaim-if-expired(key)` CAS primitive** (so ALL lease-state mutation stays here — Inv 2 no-inference); durable records surviving process death; **v1 wait policy = CAS-fail-fast, no queue, no cross-key deadlock** (single convergent key); the durable storage backing the decision-audit table.
- **Out:** deciding WHAT needs a lease (classifier); boundary enforcement; WHEN to reclaim (liveness triggers); merge of any content.
- **Owns:** 2(a), 3-durable; joint 2(b).
- **Depends-on:** daemon-arbiter-protocol

**3. manifest-classifier** — *Manifest Declaration & Classifier-Router*
- **What:** the per-repo declaration (read/scaffolded at init, never a modification) + the deterministic engine that classifies resources forkable/convergent/singular and routes them, defaulting upward when silent. The policy soul.
- **In:** the stable loader/read-model contract (sole per-project coupling point); deterministic classify-upward routing (silent → strictest); a **rock-stable classifier interface** even though the manifest schema/policy stay emergent; the declaration→**lease-key mapping** (v1: convergent key = the trunk, single key); cutting (not filling) the Layer-2 task-intake and scheduler seams.
- **Out:** modifying host config/code; the lock mechanism; enforcing/building boundaries; freezing the emergent schema; inserting any probabilistic component into the lock-decision path (binds 2(b)).
- **Owns:** 9, 11; joint 2(b).
- **Depends-on:** daemon-arbiter-protocol

### P2 — Isolation
**4. isolation-substrate** — *Forkable Isolation Substrate*
- **What:** constructs the per-agent forkable boundaries before the agent runs and produces the env-spec the launcher applies.
- **In:** per-agent git worktree as cwd; injected per-agent runtime env — explicitly including **per-agent PORT allocation and local-state paths**, not just `DATABASE_URL`/service URLs; the **grain dial** (worktree → container → VM) by conducting commodity tools (the 10-grainswap proof, and the discharge of Inv-4's "sandbox-harder" tail); worktree-FS **escape-resistance** hardening for an uncooperative agent.
- **Out:** reimplementing git/Docker/VMs; exec/PTY (launcher); the mediated remote (integrator); the singular gate.
- **Owns:** 1, 10-grainswap.
- **Depends-on:** manifest-classifier

### P3 — Live interactive governed session *(launcher operational — NOT yet "self-hosting day")*
**5. session-launcher-shim** — *Session Launcher, PTY & Transparent Shim*
- **What:** the parent-launches-child core + the transparent intercept that makes governance ambient. A forkable-only governed live session is reachable here with only isolation + ledger.
- **In:** parent execs child into the constructed env with an attached PTY (normal turn-by-turn session), attach/detach, signal/resize, and **clean-exit teardown** (release leases + signal substrate to reclaim worktree/runtime on NORMAL exit — the symmetric counterpart of launch); **mechanical boundary enforcement** (no governed action without the lease/grant); the transparent **shim-install + fail-closed interception** (a governed repo **cannot** launch an unshimmed agent → no silent Inv-4 hole); the **no-goals / no-task-dispatch** negative obligation (keeps the task-intake seam empty); the load-bearing **intake side** of the closed loop.
- **Out:** building the boundaries it launches into; cooperative hooks/MCP (adapter); the read-only surface clause; deciding env (classifier/substrate/gate produce it); the crash path (liveness).
- **Owns:** 4, 13-interaction, 12-intake.
- **Depends-on:** isolation-substrate, lease-ledger-mutex
- **Milestone:** *launcher operational* — a governed live two-agent session, isolation proven, no shared FS. (This is **not** "self-hosting day"; that is the conformance gate at P6.)

### P4 — Convergence / singular / durability (parallel after the live session)
**6. integrator-trunk** — *Mediated Trunk & Single Integrator*
- **What:** the convergent write path. Each worktree's remote points at a mad-substrate-mediated holding repo (no agent reaches real origin); the lone integrator validates and promotes to trunk.
- **In:** the mediated holding repo + redirected remote (origin-bypass **escape-resistance** is a named deliverable); an **idempotent transactional promote/rollback primitive** — explicit states `received/validating/promoted/aborted`, single atomic commit-or-rollback, exposing an idempotent `abort(integration-id)` the dead-holder path invokes, so a mid-integration death leaves trunk clean **by construction** (Inv 6/7 stay wholly inside this project); the explicit validation gate ("nothing merges silently"); the load-bearing **output side** of the closed loop; the Layer-2 validation-gate seam.
- **Out:** lease storage; richer/LLM validation content (Layer-2 plug-in to the gate seam); the single-arbiter-of-the-ledger half; the read-only display.
- **Owns:** 5-integrator, 6, 7, 12-output.
- **Depends-on:** lease-ledger-mutex, isolation-substrate

**7. singular-gate** — *Singular-Resource Default-Deny Gate*
- **What:** the default-deny boundary for resources with real external side effects.
- **In:** default-deny unless granted; granted modes (mock / mad-substrate-proxy / serialized-human-supervised); producing the env-spec for granted endpoints; serializing supervised grants via the ledger; **proxy-bypass escape-resistance** as a named deliverable.
- **Out:** deciding what is singular (classifier); the convergent trunk path; provisioning real infra; writing the child env (launcher applies the spec).
- **Owns:** 8.
- **Depends-on:** manifest-classifier, lease-ledger-mutex

**8. liveness-recovery** — *Liveness & Failure Recovery (crash path only)*
- **What:** "no lock outlives its holder; the system makes progress after any single death" — as a strict **detector + trigger**, never a mutator.
- **In:** dead-holder detection; **invoking** lease-ledger-mutex's `reclaim-if-expired`; **triggering** integrator-trunk's deterministic abort on a dead mid-integration holder; orphaned-worktree/boundary recovery; clean daemon restart reattachment from the durable ledger.
- **Out:** defining lease durability/TTL (ledger); mutating the ledger or trunk directly; clean-exit teardown (launcher owns the normal path); app-level retries/dispatch; multi-host/HA; new locking semantics.
- **Owns:** 3-reclaim.
- **Depends-on:** lease-ledger-mutex, session-launcher-shim, integrator-trunk

### P5 — Surfaces (neither is safety-load-bearing)
**9a. watch-view-surface** — *Read-Only Watch View*
- **What:** the host-agnostic Go read-only `mad-substrate watch` TUI (the "seventh terminal") restoring the combined result agents can't see live.
- **In:** read-only Bubbletea/Lipgloss TUI of integrated trunk state, pending merges, conflicts, lease holders/waiters, and the **decision-audit** stream; the **read-only surface** of the closed loop.
- **Out:** performing merges / advancing trunk; mutating any state; adding a step to the prompt loop; any per-host coupling (it is host-agnostic).
- **Owns:** 13-readonly, 12-readsurface.
- **Depends-on:** daemon-arbiter-protocol, lease-ledger-mutex, integrator-trunk

**9b. host-adapter** — *Cooperative Per-Host Layer*
- **What:** the thin per-host cooperative layer (hooks + MCP), now **native Go in the single binary** (`mad-substrate mcp` + `mad-substrate hook <event>`). **Claude Code is instance #1, not the project** — it is parameterized per host.
- **In:** translate agent MCP-tool calls → daemon JSON-RPC for claim, request-merge, see-locks (optimization + smooth UX only); the agent-facing MCP dialect kept separate from the daemon API (10-decoupling). (Historical: an early plan also intercepted each edit with a proactive "claim-before-edit" PreToolUse hook; that per-edit hook was removed — the cooperative layer is now the MCP tools plus a SessionStart standing-guidance hook, with no per-edit interception.)
- **Out:** owning ANY structural safety invariant (the substrate floor holds for an agent that calls nothing); coupling the agnostic core to one host's specifics; a general adapter SDK for N hosts (Layer-2).
- **Owns:** — (cooperative; no Layer-1 invariant).
- **Depends-on:** daemon-arbiter-protocol

### P6 — Proof & ship
**10a. conformance-harness** — *Executable Safety Authority (defines self-hosting day)*
- **What:** the executable definition of "correct" and the **sole definer of self-hosting day**.
- **In:** the safety-property conjunction — (a) no cross-visibility of forkable state **and no cross-agent coordination channel**; (b) no convergent write without exclusive lease + validated integration; (c) no singular effect without a grant; the **escape-resistance** adversarial conjunct over ALL THREE surfaces (worktree-FS escape, mediated-remote bypass, singular-proxy bypass); the **no-goals/no-dispatch** check; the **no-LLM-anywhere-in-lock-path** (2b) check; the two-agent / isolated-worktree / one-lease / mediated-integration E2E **acceptance gate**.
- **Out:** multi-host/Redis; CI of day-to-day code beyond conformance; packaging.
- **Owns:** the safety-property conjunction (no single invariant; owns the conjunction + the adversarial escape tests).
- **Depends-on:** all capability projects (5, 6, 7, 8, 9a, 9b)

**10b. distribution-packaging** — *Release Engineering*
- **What:** ship mad-substrate as one artifact.
- **In:** single static **cgo-free** Go binary (modernc.org/sqlite); version-pinning conducted substrate tools; the cooperative layer ships *inside* this binary (native Go — no separate adapter bundle); brew later; macOS + Apple Silicon first.
- **Out:** multi-host infra; auto-update of governed projects; a GUI installer; any safety-authority code (that is 10a — different cadence, different failure mode).
- **Owns:** — (release engineering; no invariant).
- **Depends-on:** the capability projects; may consume the packaged artifact

---

## Build ordering (foundation-first, acyclic, spine-rooted)
```
P0  daemon-arbiter-protocol
P1  ├─ lease-ledger-mutex        ┐ (parallel, spine-only deps)
    └─ manifest-classifier       ┘
P2  isolation-substrate          (← manifest-classifier)
P3  session-launcher-shim        (← isolation-substrate, lease-ledger-mutex)   ▶ LAUNCHER OPERATIONAL
P4  ├─ integrator-trunk          (← lease, isolation)
    ├─ singular-gate             (← classifier, lease)
    └─ liveness-recovery         (← lease, launcher, integrator)
P5  ├─ watch-view-surface        (← daemon, lease, integrator)
    └─ host-adapter              (← daemon)
P6  ├─ conformance-harness       (← all capability projects)   ▶ SELF-HOSTING DAY (acceptance gate passes)
    └─ distribution-packaging    (← capability projects)
```
- **No cycles:** the mediated remote is injected into the launcher's env as a later segment, never a build-time ancestor; `liveness-recovery → integrator-trunk` is a one-way dependency (integrator does not depend on liveness).
- **Two milestones, disambiguated:** *launcher operational* (P3) = a governed live session exists; *self-hosting day* (P6) = the conformance acceptance gate passes and mad-substrate can govern its own development against a proven safety property.

## v1 simplifications (stated so "clean seams" aren't left implicit)
- **Convergent lease key = the trunk** (single key); declaration→key mapping owned by manifest-classifier; finer granularity is the named Layer-2 scheduler seam.
- **Wait policy = CAS-fail-fast**, no queue, no cross-key deadlock (single key); the surface reports "held by session X."
- **Decision-audit log** = a durable table written via the daemon's audit interface by every decision project, read by watch-view-surface — never by importing the lease store. The write is **implemented by lease-ledger-mutex behind the daemon's registered audit method**, so the build edge stays lease→daemon (no P0→P1 dependency).

## Open questions (carried forward — emergent, not blockers)
- **Manifest schema & tier boundaries** (GROUNDING/0002): exact v1 declaration format and where forkable/convergent/singular actually fall (e.g. is a project's local Postgres a forkable copy or a singular gate by default?). The classifier ships a stable interface while this churns.
- **Integration-view behavior** (0001): continuous auto-merge vs gated; how conflicts surface and who resolves; whether the view runs the integrated app live; how "waiting on a lease" is shown.
- **Socket authz mechanism on macOS**: peer-cred check vs per-session token — the trust root the whole ledger rests on.
- **Adapter SDK extraction**: when the per-host adapter contract is generalized to N hosts (must be later, not a rewrite — 10-decoupling protects this).
- **Grain-escalation policy**: the trigger/trust policy for moving worktree → container → VM (v1 is worktree only).
