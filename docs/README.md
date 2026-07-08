# mad-trellis documentation index

This folder mixes two very different kinds of document. Read the right one for your goal.

> **If you are here to *use* mad-trellis, do not start in the numbered `0001`–`0008` files.**
> Those are **design-history records** (architecture decision records), not user guides. In
> particular [`0004-build-brief.md`](./0004-build-brief.md) is a *build-order work plan* for the
> agent that originally constructed the project — following it as a how-to will only confuse you.
> Start with the [top-level README](../README.md) and the [comparison](./comparison.md).

---

## Using mad-trellis

Practical, current, task-oriented material:

- **[Quickstart + install](../README.md#quickstart)** — the top-level `README.md`. How to install
  the binary, the governed loop, the isolation grains, and the `mad-trellis conform` safety gate.
  This is the front door.
- **[comparison.md](./comparison.md)** — an honest competitive comparison of mad-trellis vs.
  plain `git worktree`, Dagger `container-use`, devcontainers, agent sandboxes/runners, and CI
  merge-queues — including "why not just X" for each, and when you *don't* need a substrate.
- **[GROUNDING.md](../GROUNDING.md)** — the constitution: the 13 invariants every change (and every
  comparison above) is scored against. Short, load-bearing, and worth reading before anything else.
- **[CONTRIBUTING.md](../CONTRIBUTING.md)** — dev setup, how the safety gate works, and the PR
  process, for when you move from using to changing the project.
- **[containerfiles/](./containerfiles/)** — reference Containerfiles and exact build commands for
  the bring-your-own-image container grain (companion to ADR 0006 below).

## Design history / ADRs

**These are architecture-decision records — a snapshot of *why* the project is shaped the way it
is, captured at the time each decision was made. They are NOT user guides and may describe build
sequencing or historical state rather than how to operate the tool today.** Read them to understand
intent and rationale, not to learn the workflow.

- **[0001 — Form & Architecture](./0001-form-and-architecture.md)** — why mad-trellis is a
  standalone headless Go core + native cooperative layer, and the three boundaries it owns.
- **[0002 — Tech Stack](./0002-stack.md)** — the deliberately early-locked stack (Go, embedded
  SQLite ledger, Unix-socket JSON-RPC, single static binary) and the rationale for each choice.
- **[0003 — Project Breakdown (the "What")](./0003-project-breakdown.md)** — the decomposition into
  projects and the invariant-clause ownership map (every invariant clause has exactly one owner).
- **[0004 — Build Brief (the work order for BridgeSwarm)](./0004-build-brief.md)** — the original
  parallelized **build plan** handed to the agent that constructed mad-trellis. **History, not a
  guide** — do not read this as instructions for using the tool.
- **[0005 — The governed trunk loop (self-hosting topology)](./0005-governed-trunk-loop.md)** — how
  mad-trellis develops itself through its own mediated integrator (the dogfood topology).
- **[0006 — The container-grain image contract (bring-your-own-image)](./0006-container-grain-image-contract.md)**
  — the contract a container image must satisfy, why mad-trellis ships no image, and the grain's
  env knobs.
- **[0007 — Event nudges for the integration review loop](./0007-event-nudges.md)** — why
  daemon-authored wake-ups exist, how launcher/MCP delivery works, and why nudges never become
  agent-authored messages or task dispatch.
- **[0008 — The death oracle, quarantine, and salvage-before-destroy](./0008-death-oracle-and-salvage.md)**
  — (PROPOSED) the liveness redesign after the 2026-07-05 sleep/dark-wake false-death incident:
  suspension-aware lease clock, connection reality as a destruction veto, resurrectable sessions,
  and two-phase reclaim where work is salvaged into a git ref before anything is ever removed.

---

*New here? Read [GROUNDING.md](../GROUNDING.md), then the [README quickstart](../README.md#quickstart),
then [comparison.md](./comparison.md). Reach for the ADRs only when you want the "why."*
