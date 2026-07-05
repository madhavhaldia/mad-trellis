# mad-substrate — Grounding Invariants

*The constitution. When an implementation decision is unclear, the answer is here.*
*If a feature can't be reconciled with these, the feature loses — not the invariant.*

---

## Mission (one sentence)

**However you drive your agents, the road can't kill you.**
We govern the substrate beneath agentic work so that no agent can corrupt another agent
or the trunk — without dictating how the work is done.

We are a **governance substrate, not an orchestrator.** BridgeSwarm (and tools like it) are
one opinion about *how to drive* agents. We are the opinion that *however you drive them, the
work can't be corrupted.* We sit **under** any agent, any orchestrator, any stack — including
under an orchestrator like BridgeSwarm.

---

## The model everything routes through

Every resource an agent can touch is **exactly one** of three kinds. Classification is the
first act of every decision.

| Kind | Definition | Mechanism |
|------|------------|-----------|
| **Forkable**   | Can be *copied* per agent (source, runtime, ports, local state) | **Isolate** — give each agent its own |
| **Convergent** | Must reduce to *one ordered truth* (schema/migrations, lockfiles, the trunk) | **Lease + integrate** — serialize authorship, reconcile |
| **Singular**   | One *external* identity with real side effects (prod data, SaaS, accounts, rate-limited APIs) | **Gate** — mock, forbid, or serialize under human supervision |

The three kinds form a **safety ladder**: Forkable (loosest) → Convergent → Singular (strictest).
Errors are asymmetric:
- Misclassifying **upward** (treating a looser thing as stricter) costs *parallelism*. Annoying.
- Misclassifying **downward** (treating a stricter thing as looser) causes *corruption or real-world damage*.

The entire engineering problem is **classification + routing**. The lock is a `SETNX`; knowing
*what needs* the lock is the whole game.

---

## The Invariants

### Isolation
1. **No two agents ever share a writable forkable resource.** If it can be copied, it is copied —
   code, runtime, ports, local state. An agent's in-progress work is invisible to every other
   agent except through the integration plane.

### Coordination
2. **Mutual exclusion is deterministic, never inferred.** Lock acquisition is an atomic
   compare-and-swap in code. No probabilistic or LLM component is *ever* in the critical path of
   "who holds exclusive access."
3. **Locks are leases; the ledger is durable.** Every lock has a TTL and is renewable only by a
   live holder. The record survives any process death. **No lock outlives its holder** — the
   system always makes progress after any single agent dies.
4. **Enforce at the boundary; never trust.** An agent cannot perform a governed action without
   holding the lease, and the check is mechanical at the action boundary — not a request the agent
   may ignore. An agent that can't be enforced is treated as untrusted and sandboxed harder.
5. **One arbiter, one integrator — star, never mesh.** Agents never coordinate peer-to-peer.
   There is a single source of truth for the ledger and a single authority for promotion to trunk.

### Convergence & Integration
6. **Convergent resources are single-writer, and nothing merges silently.** Every convergence
   passes through an explicit integration step with validation. No automatic merge without a gate.
7. **The trunk only ever advances through validated integration.** No agent writes the trunk directly.

### Singular resources
8. **The world is default-deny.** Anything with external side effects is forbidden to agents
   unless explicitly granted; grants are serialized and/or human-supervised.

### The master rule
9. **Classify upward under uncertainty.** Unknown kind → treat as the stricter kind. The system
   may be too conservative; it may never be too permissive.
   **Lost parallelism is acceptable; corruption is not.**

### Boundaries that must never blur
10. **Mechanism is swappable; policy is the product.** The substrate (worktree / container / VM),
    the agent (Claude / Codex / human), and the orchestrator are all interchangeable. The
    classifier and the governance rules are the invariant core. We couple to none of them.
11. **Project coupling is a declaration, never a modification.** We read what a project *declares*
    about its resources. We never require a project to change its config or code to be governed.
12. **No airlocks. The loop is closed.** Any capability that produces inputs for agents (a plan, a
    task list) or consumes their outputs (a review verdict, a merge) must operate *in place*. The
    system never forces a manual handoff to or from another tool. If using a feature requires
    copy-pasting across an app boundary, it's a design failure, not a feature.

### Experience
13. **Governance is ambient; interaction is unchanged.** Using mad-substrate is identical to using a
    bare interactive agent session — open a terminal, run your agent, prompt it live, turn by turn.
    mad-substrate takes **no goals and dispatches no tasks**; the user drives each session. Any design
    that makes the user *operate* mad-substrate instead of *just using their agent* is wrong. The only
    new surface mad-substrate may add is a **read-only view of the integrated result**; it never adds
    steps to the prompt loop. Substrate-authored coordination NUDGES — fixed-template, daemon-authored
    signals about governed state (a pending review, a verdict), delivered by the launcher/MCP layer
    and disableable via `MAD_NUDGES=off` — are ambient governance, not goal dispatch; they may never
    carry agent-authored content or task instructions.

---

## Scope & Layering

**Layer 1 — The Foundation (this document).** The governance substrate: isolation, leasing,
integration, classification. We build this first, completely, and never compromise it.

**Layer 2 — Designed-for extensions.** Genuinely valuable, *not* core, and **not excluded.** Each
plugs into a seam the foundation exposes. None may be built before Layer 1 holds, and none may
modify an invariant — they *consume* the foundation's interfaces.

| Extension | The seam it plugs into | What it actually is |
|-----------|------------------------|---------------------|
| Planning / decomposition | The **task-intake** interface | An automated *producer* of "agent does X." The foundation already runs X safely. |
| Correctness / quality review | The **integration validation gate** | A richer *validator*. The gate already blocks unvalidated convergence. |
| Throughput / parallelization tuning | The **lease / scheduler policy** | An *optimization* of how finely we classify and lease. |

**Design the seams now; fill them later.** The reason these become no-brainers is that a
well-built foundation already has a socket for each. The broken alternative — plan in another app,
copy-paste into the agents here — is exactly what Invariant 12 forbids.

**The sequencing law.** A Layer-2 feature is never a reason to weaken a Layer-1 invariant.
"This extension is such a no-brainer" is exactly when the invariants are most at risk — that's
when they bind hardest, not least. Foundation first *and complete*; extensions strictly after.

---

## The decision oracle (when stuck, ask in order)

1. What *kind* of resource does this touch? Route to its bucket.
2. If I'm unsure of the kind — did I pick the **stricter** one?
3. Is any probabilistic / LLM component in a **correctness-critical** path? If yes, stop and redesign.
4. Does this require the user to **change** their project, or only to **declare** something?
5. If an agent died right now holding this, does the system **recover**?
6. Am I solving **collision-safety** — or have I drifted into orchestration / planning / quality?
   If the latter, it's Layer 2 — note the seam and move on.

---

## The safety property (definition of "correct")

At all times, for any set of concurrently running agents, no agent can:

- **(a)** observe or corrupt another's in-progress forkable state;
- **(b)** write a convergent resource without an exclusive lease *and* validated integration;
- **(c)** affect a singular resource without an explicit grant.

The trunk only ever advances through validated integration.

**Every feature must preserve this property. Anything that can't is rejected.**

---

## Load-bearing notes

- **The non-goals became Layer 2, not exclusions** — but the *sequencing* discipline is the same
  guardrail it always was. We don't build Layer 2 as, or before, the foundation.
- **Invariant 9 is the soul.** Conservative under uncertainty; serialization is acceptable,
  corruption never is. Almost every hard call collapses to this line.
- **Invariants 2 and 10 are where products like this rot.** The temptation to let an LLM "just
  decide" a lock (violates 2), or to get smarter by coupling to one stack (violates 10), is
  constant. They are absolutes on purpose.
- **"No-brainer" is the phrase that precedes scope creep.** The seductiveness of Layer-2 features
  is itself the hazard. The temporal discipline — foundation first and complete — is the
  guardrail.
