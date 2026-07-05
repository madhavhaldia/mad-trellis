# mad-trellis vs. the alternatives

*An honest competitive comparison. mad-trellis is a **governance substrate for parallel
agentic development** — it sits **under** whatever drives your agents and guarantees that no
agent can corrupt another agent or the trunk. It is **not** an orchestrator, **not** an agent,
and **not** a container tool. That framing is what most of the comparisons below turn on: the
alternatives are usually solving a different (often narrower, often simpler) problem, and for
many people that narrower problem is all they have.*

Read [GROUNDING.md](../GROUNDING.md) first — the 13 invariants are what the table is scored
against, and the [README quickstart](../README.md#quickstart) shows the actual workflow.

---

## What is genuinely hard to copy

Three properties below are easy to *claim* and hard to *build*. They are the reason this project
exists, and they are where every alternative is weakest. Lead with these when deciding:

1. **A falsifiable safety gate (`mad-trellis conform`).** The safety claims are not prose in a
   README — they are an executable, AND-not-OR gate. Every safety clause must pass, and **each
   clause ships with a non-vacuous control that injects the violation and proves the check flips
   red.** A check that stays green without proving it can fail is treated as the cardinal defect.
   No competitor below ships an adversarial self-test of its own safety model; their guarantees
   are documentation, configuration, or convention. This is the differentiator that is hardest to
   fake: you can *run* it and watch it go red.

2. **The forkable / convergent / singular model.** Every resource an agent can touch is classified
   as exactly one of three kinds — **forkable** (copy per agent), **convergent** (serialize +
   validate-merge), or **singular** (default-deny gate) — and the *whole* engineering problem is
   classification and routing, with a hard rule to **classify upward** under uncertainty (lost
   parallelism is acceptable; corruption never is). The alternatives give you isolation (forkable)
   and maybe a merge step (convergent), but none of them treats *singular* external side effects
   (prod data, SaaS accounts, rate-limited APIs) as a first-class default-deny class, and none
   forces the upward-classification discipline.

3. **Crash-reclaim leasing.** Mutual exclusion is a deterministic atomic compare-and-swap (never
   inferred, never an LLM in the critical path), every lock is a **lease with a TTL renewable only
   by a live holder**, the ledger is durable across process death, and **no lock outlives its
   holder** — the system makes progress after any single agent crashes. A dead agent never wedges
   the trunk. Most alternatives either don't coordinate writers at all or rely on a human/CI to
   notice a stuck job.

Everything else — agent-agnostic, not-an-orchestrator, interaction-unchanged — follows from the
invariants and is summarized in the table.

---

## The table

| Capability | **mad-trellis** | plain `git worktree` | Dagger `container-use` | devcontainers | agent sandboxes / runners (OpenHands, SWE-agent, a coding-agent's own sandbox) | CI merge-queue (GitHub/GitLab/Mergify) |
|---|---|---|---|---|---|---|
| **Per-agent isolation** (FS, runtime, ports, local state) | Yes — grain dial: worktree (default) or container/structural cap-drop confinement | Filesystem + branch only; shared runtime, ports, local state | Yes — container per environment | Yes — container per dev/agent | Yes — sandbox per agent/session | No (isolation is the runner's job, not the queue's) |
| **Safe *concurrent* trunk writes** (many agents, one trunk) | Yes — single integrator, lease-gated, validated, one atomic CAS; trunk advances no other way | No — concurrent merges race; you serialize by hand | No — convergence/merge is out of scope | No — out of scope | No — usually one agent → one PR; collisions handled downstream | Yes — *that is its whole job* (serializes PR merges, re-tests) |
| **Crash recovery / no stuck locks** | Yes — TTL leases, durable ledger, reclaim on holder death; no lock outlives its holder | N/A — no locks to recover; manual cleanup of stale worktrees | Partial — container lifecycle, not a lease ledger | Partial — restart the container; no governance state | Varies — runner may orphan a session; rarely a reclaim guarantee | Queue may stall on a stuck job; human/timeouts unstick it |
| **Deterministic, *falsifiable* safety** | Yes — `mad-trellis conform`: AND-not-OR, every clause has a control that proves it can go red | No | No — correctness is the user's pipeline | No | No — safety is sandbox policy + prompt, not a self-test | Partial — "did tests pass" is checkable, but no adversarial proof of the *queue's own* safety |
| **Agent-agnostic** (Claude / Codex / human, swappable) | Yes — couples to no agent; cooperative layer is advisory and fail-soft | Yes — it's just git | Mostly — tool-driven, but oriented to its own flow | Yes — any tool in the container | No — each is *its own* agent/runner | Yes — agent-agnostic (operates on PRs) |
| **Not an orchestrator** (takes no goals, dispatches no tasks) | Yes — by design; you drive each session, it only adds a read-only view | Yes | Mixed — environment tooling, leans toward driving | Yes | No — orchestration *is* the product | Yes — it sequences merges, doesn't drive work |
| **Default-deny on singular side effects** (prod, SaaS, rate-limited APIs) | Yes — first-class singular gate (Inv 8) | No | No | No | Partial — network/secret policy, not a classified gate | No |
| **Reads project declarations without modifying the project** | Yes — `mad-trellis.json`; coupling is a declaration, never a code change | N/A | Requires its config/wiring | Requires `devcontainer.json` | Requires adopting the runner's harness | Requires queue config + branch protection |
| **External infra required** | None (single static cgo-free binary + git) | None | Dagger engine / container runtime | Container runtime | Runner infra / service | CI service + hosted forge |

Scores are about *defaults and design intent*, not "could you bolt this on." Almost anything can
be scripted into almost anything; the point is what the tool *guarantees* out of the box.

---

## Why not just…

### …plain `git worktree`?
For one agent, or a few agents you personally babysit on clearly disjoint files, `git worktree` is
genuinely enough — and it is zero install, zero concept, already in your toolbelt. mad-trellis's
default grain is *built on* worktrees precisely because they are the right primitive. What plain
worktrees do **not** give you: any coordination of who may write a convergent resource (two agents
editing the same lockfile or migration chain still race), any gate on trunk advancement (a bad
merge lands directly), any lease/crash-recovery story (a stalled agent just leaves a stale tree),
and any default-deny on real-world side effects. Worktrees isolate the *forkable*; they say nothing
about the *convergent* or *singular*. If your parallelism is low and your discipline is high, that
silence is fine. As soon as N agents contend for one trunk unattended, the silence is the bug.

### …Dagger `container-use`?
`container-use` gives each agent a clean, reproducible containerized environment with good
tooling — strong on the **forkable** axis, and arguably more ergonomic for "spin up an env" than
mad-trellis's container grain. But it is an *environment* tool, not a *governance* substrate: it
does not own a lease ledger, it is not the sole validated promoter of your trunk, it has no
crash-reclaim guarantee that "no lock outlives its holder," and it ships no falsifiable safety gate
that adversarially proves its own isolation can be detected when broken. If your problem is "give
each agent a good sandbox," `container-use` may be the simpler, better-fitting answer. If your
problem is "many agents must converge onto one trunk without corrupting it, and a dead agent must
never wedge anything," that is a different problem and container-use does not claim to solve it.

### …devcontainers?
Devcontainers are the mature, ubiquitous standard for *reproducible, isolated development
environments*, and if your need is "every agent/dev gets the same toolchain in a container," they
are simpler, better supported, and editor-native — use them. They are also **orthogonal** to
mad-trellis rather than competing: a devcontainer is one way to realize a forkable boundary.
What a `devcontainer.json` will never be is a coordinator. It has no concept of a lease, no single
integrator, no validated atomic trunk advance, no default-deny gate on singular resources, and no
self-falsifying safety test. Devcontainers answer "what environment," mad-trellis answers "who
may write what, when, and how does it merge safely" — you can use both.

### …an agent sandbox/runner (OpenHands, SWE-agent, a coding agent's own sandbox)?
These are the closest in spirit *and* the most different in kind, so be precise about the overlap.
They sandbox an agent (forkable isolation — often very good) **and** they orchestrate it: they take
a goal, plan, dispatch steps, and drive the loop. mad-trellis deliberately does the opposite of
the second half — it takes **no goals and dispatches no tasks** (Inv 13), and is meant to sit
*underneath* exactly these tools. So this is not "mad-trellis instead of OpenHands"; it is
"OpenHands (or SWE-agent, or your agent's native sandbox) *on top of* mad-trellis." What the
runners' own sandboxes don't provide: cross-agent trunk convergence (each typically produces one
PR and lets the forge sort out collisions), a durable lease ledger with crash reclaim spanning
multiple independent agents, a default-deny singular gate, and a falsifiable safety conformance
suite. If you run a single agent through one runner and let humans/CI merge its PRs, you may never
need a substrate. The substrate earns its keep when *multiple, possibly heterogeneous* agents run
in parallel against one trunk.

### …a CI merge-queue (GitHub/GitLab/Mergify)?
A merge-queue is the strongest alternative on the **one** axis it targets: serializing many PRs
into a trunk, re-testing each against the latest, and only fast-forwarding green ones. For a team
of humans (or agents) that already produce PRs and whose only contention is "the trunk," a merge
queue is mature, hosted, and often *sufficient* — and mad-trellis does not replace your CI
validation; it can call it. The differences are scope and locus. A merge-queue acts at the **PR
boundary, after the fact, in your forge**; it does nothing about *per-agent isolation while the
work happens* (no shared-runtime/port/local-state separation), nothing about *singular* side
effects an agent triggers mid-run, and nothing about an agent that crashes holding contended local
state. Its "safety" is "tests passed," which is checkable but is not a falsifiable proof of the
queue's *own* coordination guarantees. mad-trellis governs the **whole lifecycle** — isolate
while working, lease the convergent, gate the singular, then promote through a validated atomic CAS
— and a merge-queue can live happily as the *validator* the integrator calls. They compose more
than they compete; if all you contend over is trunk merges, the queue alone may be enough.

---

## When you do *not* need mad-trellis (said plainly)

- One agent at a time, or agents on provably disjoint files you watch directly → `git worktree`.
- You only need clean per-agent environments → devcontainers or `container-use`.
- You run a single agent through one harness and merge its PRs by hand/CI → that harness + a
  merge-queue.

mad-trellis earns its complexity exactly when **multiple, possibly heterogeneous, possibly
unattended agents** must converge on **one trunk** and you need a *provable* guarantee — not a
hope — that none of them can corrupt another or the trunk, and that a crash never wedges the
system. If that is not your situation, a simpler tool is the right call, and saying so is the
honest answer.
