# 0001 — Form & Architecture

*Status: accepted. Companion to [GROUNDING.md](../GROUNDING.md). Where the invariants say*
*what must be true, this says what we build to make it true.*

---

## Decision

mad-trellis is a **standalone, headless governance runtime** — a CLI + daemon that owns the
isolation substrate, the lease ledger, and the integration gate. It is **not** a GUI app, and
**not** a plugin embedded inside one host.

- It ships with a thin, **native-Go cooperative layer** (first host: Claude Code) — the `mad-trellis mcp` / `mad-trellis hook` subcommands built into the binary, no separate toolchain.
- A GUI app, if it ever exists, is Layer-2 and last — a face on the engine, never a prerequisite.

This is forced by the invariants: coupling to no agent/host/UI (Inv. 10), enforcing at a boundary
we own rather than trusting the agent (Inv. 4), and building the foundation before any surface
(the sequencing law). A full app would mean building commodity mechanism (terminal panes, agent
runner, UI) before proving the classifier + lease + integration core that is the actual product.

## Interaction model — governance is ambient (Inv. 13)

The UX is identical to bare interactive sessions. There is no "spawn a task" workflow.

1. `mad-trellis init` — one-time per repo. Reads/creates the manifest (declared forkable /
   convergent / singular resources). The *only* per-project coupling (Inv. 11).
2. After init, launching **any supported agent CLI** (`claude`, `codex`, the pydev agent, …) in
   that repo **auto-launches it inside a mad-trellis-governed environment** — via a transparent shim.
   The user types their normal command and gets their normal live, interactive session.
3. Open six terminals, run your agent in each, prompt each freely and forever. mad-trellis is
   invisible. Agents cannot collide; the user never operates mad-trellis to get that.

The unit of interaction is the **session** (you drive each, live), never the **goal** (the
BridgeSwarm model we reject). mad-trellis decides *nothing* about what agents do.

## Mechanics — parent launches child

mad-trellis is the **parent process**; the agent CLI is a **child it `exec`s into an environment it
built.** Owning the *wrapper* (the execution environment) is what gives us boundaries — and the
wrapper is a process-launcher + sandbox, **not** a UI. Before the agent starts, mad-trellis
constructs the boundaries:

| Resource kind | Boundary mad-trellis builds | Enforcement |
|---------------|---------------------------|-------------|
| **Forkable** (code, FS) | A per-agent git worktree (or container/VM); the agent's cwd | Siblings' files don't exist in the agent's view |
| **Convergent** (trunk) | The worktree's git remote points at a **mad-trellis-mediated holding repo**, not real origin | Agent has no remote that reaches the trunk; only the integrator does |
| **Singular/runtime** (DB, services) | Injected env (`DATABASE_URL`, service URLs) → per-agent copy or a mad-trellis proxy | Agent can only reach what it was routed to |

Boundaries are **structural**, not watched. The agent simply cannot reach what it wasn't given.

## Two layers: substrate (hard) vs adapter (soft)

| Layer | Owned by | Role | Works without agent cooperation? |
|-------|----------|------|----------------------------------|
| **Substrate** | mad-trellis launcher | The hard boundaries above | **Yes** — structural, unbypassable |
| **Cooperative layer** | native-Go per-host shim (hooks + MCP), built into the binary | Lets the session talk to mad-trellis: claim before edit, request merge, see others' locks, closed-loop UX | No — cooperative, optimization only |

Safety never depends on the adapter. The adapter buys *proactive* conflict avoidance and smooth UX
(Inv. 12); the substrate is the floor that holds even for an agent that calls nothing (e.g. Codex,
which can't be hook-gated).

## Isolation grain scales by trust

The model "launch the agent into an environment I built" is identical whether the environment is a
**worktree** (cheap, trusted), a **container** (isolated services), or a **VM** (blast-radius,
disposable snapshots). The grain is a dial; the architecture does not change. Own the
*orchestration* of these primitives — never reimplement Docker.

## The one new surface: the integration view

Isolation means agents don't see each other's work live (accepted — it's why they can't collide).
The combined, running result is restored by a **read-only integration view** (`mad-trellis watch` —
the "seventh terminal" / continuous-integration agent). This is the only surface mad-trellis adds
beyond the agent sessions, and it is read-only by Inv. 13.

**Status: required, design deferred.** Open questions: continuous auto-merge vs. gated; how merge
conflicts surface and who resolves them; whether the view runs the integrated app live; how
"agent is waiting on a lease" is shown. This is the next design topic.

## Accepted trade-offs

- **Headless is hard to demo.** No visible cockpit like BridgeSpace. Mitigate with strong CLI UX
  and the integration view; do not pretend the go-to-market weakness isn't real.
- **Adapter treadmill.** N hosts × shifting extension APIs. Mitigate by leading with substrate-level
  enforcement so adapters are enhancements, not requirements.
- **"Own the substrate" must not become "rebuild Docker."** Conduct commodity isolation primitives;
  don't reimplement them.
- **Thin-wrapper risk.** If the engine is just process-spawning, it's a weekend script. Defensible
  depth lives in the classifier, lease semantics, and integration intelligence — guard that.

## Open questions / next

- Detailed design of the integration view (above).
- The manifest schema (what a project declares, and how the classifier defaults when it's silent).
- First falsifiable build target: the lease service (Inv. 2–5) and/or a minimal end-to-end
  (two agents, isolated worktrees, one convergent lease, mediated integration).
