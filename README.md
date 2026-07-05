# mad-substrate

**No global lock — every agent runs free; the substrate keeps the trunk safe.**

> However you drive your agents, the road can't kill you.

mad-substrate is a **governance substrate for parallel agentic development**. Run many coding agents
against one repository without them corrupting each other or the trunk — whatever agents you use,
however you drive them.

It is **NOT an orchestrator.** It does not plan your work, decompose tasks, or decide what each agent
does. It sits *underneath* whatever agent or orchestrator you already use and guarantees exactly one
thing: **safe parallelism**.

**Who it's for:** anyone running more than one coding agent on a single repo — Claude, Codex, a
home-grown loop, a human in a worktree, or all of them at once. If you have ever had two agents
stomp each other's edits or race a merge into `main`, this is the layer that makes that impossible.

mad-substrate ships as a single static, cgo-free Go binary (an arbiter daemon + a CLI + a transparent
launcher) with a built-in, native-Go cooperative layer — no Node, no pnpm, no runtime dependency
beyond `git`.

---

## See it work

<!-- TODO: demo gif (mad-substrate watch during a 4-agent run) -->

The whole system is a **star, never a mesh** (Inv 5): one arbiter owns the ledger, one integrator is
the *only* writer of the trunk, and every agent lives in its own isolated boundary that no other
agent can observe.

```
                 agent A          agent B          agent C          agent D
              (worktree /      (worktree /      (worktree /      (worktree /
               container)       container)       container)       container)
                   |                |                |                |
                   |  lease / RPC   |                |                |
                   +--------+-------+-------+--------+-------+--------+
                            |               |                |
                            v               v                v
                  +-------------------------------------------------+
                  |          arbiter daemon  (the star hub)         |
                  |   durable lease ledger (SQLite, single-writer)  |
                  |   crash-reclaim · decision audit · classify     |
                  +-------------------------------------------------+
                            |                              ^
              submit / promote (validated, lease-gated)    | read-only
                            v                              |
                  +-----------------------+        +----------------------+
                  |   THE integrator      |        |  mad-substrate watch |
                  | (sole trunk writer)   |        |  (read-only TUI)     |
                  |  one atomic CAS via   |        +----------------------+
                  |  git merge-tree       |
                  +-----------+-----------+
                              |
                              v
                  +-----------------------+
                  |  mediated trunk (bare)|  <-- the ONLY way trunk advances (Inv 7)
                  +-----------------------+
```

`mad-substrate watch` is a seventh terminal: a read-only mirror of trunk tip, in-flight integrations,
lease holders, and the decision-audit stream. Killing it never changes a governed outcome.

## The core model

Every resource an agent touches is **exactly one** of three kinds, and each routes to exactly one
mechanism:

| kind | example | mechanism |
|------|---------|-----------|
| **Forkable** | the working tree, scratch dirs, ports | **isolate** — copy it per agent |
| **Convergent** | the trunk / shared branches, lockfiles, migrations | **lease + integrate** — serialize authorship, then validate-merge |
| **Singular** | a prod database, a payment API | **gate** — deny by default; mock, proxy, or supervise |

When you're unsure which a resource is, **classify upward.** Lost parallelism is acceptable;
corruption never is. The full set of rules lives in **[GROUNDING.md](./GROUNDING.md)** — the
constitution every design decision is held against.

## "Is this just git worktrees + a script?"

No. A `for`-loop that opens a worktree per agent gives you isolation and nothing else — the moment
two agents try to merge, you are back to races, lost writes, and a corrupted trunk. The hard part
isn't forking; it's **convergence under concurrency without trust.** mad-substrate is the part a script
doesn't have:

- **A durable single-writer lease ledger.** Mutual exclusion is a real atomic compare-and-swap in
  embedded SQLite (cgo-free via `modernc.org/sqlite`), not an inferred or advisory lock and never an
  LLM "deciding" who holds it (Inv 2). The record is durable — it survives any process death.
- **Crash-reclaim: no lock outlives its holder.** Every lease has a TTL and is renewable only by a
  live holder. If an agent dies mid-flight, liveness reclaims its lease and aborts its dead
  integration so the system always makes progress (Inv 3). A script's `flock` just deadlocks.
- **The integrator is the SOLE trunk writer.** Trunk advances through exactly one path: a validated,
  lease-gated, **single atomic merge-tree CAS** (`git merge-tree --write-tree`, hence the git ≥ 2.38
  floor). No agent — and no script — writes the trunk directly. A stale base or a conflict makes the
  promote abort cleanly, leaving the trunk byte-identical (Inv 6, Inv 7).
- **Classify-upward by default.** Unknown resources are treated as the *stricter* kind. The substrate
  may be too conservative; it is never too permissive (Inv 9). A script defaults to "permit."
- **A falsifiable safety gate.** `mad-substrate conform` boots a real governed scenario and proves the
  safety property holds — and proves each check actually fails when the violation is injected. Your
  script has no such proof. See below.

The lock is the easy `SETNX`; knowing *what needs* the lock, serializing it durably, recovering it
when a holder dies, and admitting only validated merges to the trunk is the whole game.

## Install

### Prebuilt binaries (recommended)

> **GitHub Releases — coming with v0.1.0.** Until then, use `go install` or build from source.

### `go install`

```sh
go install github.com/madhavhaldia/mad-substrate/cmd/mad-substrate@latest
```

> **Caveat:** an **untagged** `go install` builds *without* the embedded container cooperative plane,
> so the **container grain is unavailable**. For container-grain support, use `make install` (below),
> which builds and embeds the in-container relay assets.

### From source

Requires **Go 1.26+** and **git ≥ 2.38** (the integrator uses `git merge-tree --write-tree`).

```sh
git clone https://github.com/madhavhaldia/mad-substrate
cd mad-substrate
make install          # builds the cgo-free binary into ~/.local/bin (must be on your PATH)
#   make install PREFIX=/usr/local   # or a system location
mad-substrate doctor      # verify the install (paths, git floor, version pins)
```

`make install` is also how you upgrade after pulling. There is no runtime dependency beyond git;
SQLite is embedded.

### Short alias (`ms`)

Typing `mad-substrate` every time gets old. `make install` also drops an `ms` symlink alongside the
binary, so `ms launch -- claude` just works:

```sh
ms launch -- claude        # ms == mad-substrate
make install ALIAS=sub     # prefer a different name…
make install ALIAS=        # …or no alias at all
```

Installed the release binary directly (not via `make install`)? Create the alias yourself:

```sh
mad-substrate alias              # symlinks `ms` next to the binary
mad-substrate alias --print      # or print a shell-rc line: alias ms='…/mad-substrate'
```

## Quickstart

mad-substrate is **per-repo and zero-config**: each git repo automatically gets its own isolated
runtime (socket / ledger / mediated trunk) under `~/.mad-substrate/repos/<hash>/`. Just `cd` into a
project and launch an agent — **the daemon auto-starts**, and on a clean exit the launcher
**auto-converges** the work back. No environment variables, no manual daemon, no setup.

```sh
cd /path/to/your/project

# Run any agent inside an isolated, governed boundary. The daemon auto-starts;
# on clean exit, the work is auto-converged back. The agent is opaque to mad-substrate:
mad-substrate launch -- claude        # or: codex, a shell, your own tool

# Run several at once — each gets its own boundary; none can see another's edits:
mad-substrate launch -- claude        # (in another terminal)
mad-substrate launch -- codex         # (in a third)

# Watch the live governance loop (read-only):
mad-substrate watch
```

`mad-substrate launch` is **fail-closed**: if it cannot establish governance it refuses to run the
agent (it never runs it ungoverned). On clean exit it tears the boundary down and converges.

**Optional, not required:**

```sh
mad-substrate init          # write mad-substrate.json to declare what's forkable/convergent/singular
mad-substrate daemon        # start the arbiter by hand (launch auto-starts it otherwise)
mad-substrate spawn         # just open an isolated worktree to work in yourself (no agent)
```

`init` only refines classification (sane defaults apply without it); `daemon` is auto-started by
`launch`. Neither is needed to get going.

## Isolation grains

The *strength* of isolation is a dial (Inv 10), selected with `--grain` or `MAD_GRAIN`:

- **`worktree`** (default, **cross-platform**) — a git worktree per agent, outside the repo, with a
  disjoint port block and private scratch/cache/state. Escape-resistance is grain-relative
  (mad-substrate never *hands out* a path that escapes the boundary).
- **`container`** — **structural** confinement of an *uncooperative* agent via a Linux container
  (Apple `container`; **macOS / Apple-silicon only**): full cap-drop, read-only rootfs,
  `--network none` by default, only the agent's own clone + state mounted. The host filesystem, the
  canonical trunk, the daemon socket, and network egress are all structurally unreachable. Egress is
  one opt-in knob (`MAD_CONTAINER_NETWORK=default`) for a trusted agent that must reach its API.
  Requires `make install` (the embedded relay assets); see the `go install` caveat above.

## Safety: `mad-substrate conform`

`mad-substrate conform` is the executable safety authority — and it is **falsifiable**. It boots a
real governed scenario through the public daemon contract + CLI **only** (a hermetic scratch daemon —
it never touches your real runtime) and exits non-zero on any safety-clause failure:

```sh
mad-substrate conform   # exit 0 = GREEN = safe to self-host; non-zero = RED
```

It asserts the full safety property as a **conjunction (AND-not-OR)**: (a) forkable isolation with no
coordination channel, (b) no convergent write without an exclusive lease *and* validated integration,
(c) no singular effect without a grant — plus a two-agent / one-lease / mediated end-to-end gate.

Crucially, **every clause carries a non-vacuous control** that injects the violation and proves the
check flips red. A green that doesn't prove its clause can actually fail is the cardinal defect this
gate is built to prevent. The printed coverage matrix maps each assertion to its invariant and owning
component. Container-grain rows skip (with a reason) when no container runtime is present, so the gate
stays runnable on any host.

## Status

**Early but functional and self-hosting** — mad-substrate develops itself through its own governed
trunk loop (see Advanced, below). The CLI, daemon, isolation grains, and the `conform` safety gate
all work today.

**Pre-1.0:** the `mad-substrate.json` manifest schema and some CLI flags may still change. Not yet
published to a package registry — install via `go install` or from source (above).

## Advanced: the governed trunk loop (self-hosting)

> You do **not** need this to use mad-substrate. The Quickstart above is the everyday path. This
> section is for self-hosting onto a **mediated trunk**, where the integrator is the sole trunk
> writer and every advance is validated, lease-gated, and observable in `watch`.

Trunk only ever advances through the integrator — a validated, lease-gated, single atomic
compare-and-swap (Inv 7). The mediated trunk is a bare repo mad-substrate owns; a receive hook lets
agents push only `refs/heads/nm/*` and **rejects** any push to `trunk`.

**One-time setup** (self-contained — this is what the old quickstart forgot to create):

```sh
# 1. Start a standing daemon. It auto-creates the mediated trunk repo + receive hook.
mad-substrate daemon &

# 2. Seed the mediated trunk from your current history (a local ref write, not an agent push).
#    Replace <repo> with your repo path and <branch> with your trunk branch (e.g. main):
git -C ~/.mad-substrate/trunk.git fetch <repo> +<branch>:refs/heads/trunk

# 3. Add the mediated repo as a remote named `med` in your working checkout:
git -C <repo> remote add med ~/.mad-substrate/trunk.git
```

**The loop** (every change), now that `med` exists:

```sh
git checkout -b nm/<slug>                         # isolate the work (or: mad-substrate spawn)
# ...edit, commit on nm/<slug>...
git push med nm/<slug>                            # publish to the mediated trunk (hook allows nm/*)
id=$(mad-substrate trunk submit refs/heads/nm/<slug>)  # -> integration id (received)
mad-substrate trunk promote "$id"                      # validate + lease + atomic advance -> promoted
git fetch med && git merge --ff-only med/trunk    # follow trunk in your checkout
```

A merge conflict or stale base makes `promote` abort cleanly (trunk untouched); resubmit against the
new tip. A mid-promote death is reconciled by ancestry on the next `promote` / `status` / `recover`.
The full topology is in **[docs/0005-governed-trunk-loop.md](./docs/0005-governed-trunk-loop.md)**.
Integrator and builder sessions can also receive fixed-template, daemon-authored nudges when review
state changes; those nudges are wake-ups only, and the durable integration rows remain the source of
truth. See **[docs/0007-event-nudges.md](./docs/0007-event-nudges.md)**.

## Docs

**Using mad-substrate**

- **[GROUNDING.md](./GROUNDING.md)** — the 13 invariants and the forkable/convergent/singular model. Read this first.
- **[docs/0005-governed-trunk-loop.md](./docs/0005-governed-trunk-loop.md)** — the self-hosting / mediated-trunk loop.
- **[docs/0006-container-grain-image-contract.md](./docs/0006-container-grain-image-contract.md)** — the container-grain image contract.
- **[docs/0007-event-nudges.md](./docs/0007-event-nudges.md)** — daemon-authored wake-ups for the integration review loop.
- **[CONTRIBUTING.md](./CONTRIBUTING.md)** — dev setup, the safety gate, and the PR process.

**Design history (ADRs)**

- **[docs/0001-form-and-architecture.md](./docs/0001-form-and-architecture.md)** — the shape: headless Go core + native cooperative layer; the three boundaries.
- **[docs/0002-stack.md](./docs/0002-stack.md)** — the pinned stack and dependency rationale.
- **[docs/0003-project-breakdown.md](./docs/0003-project-breakdown.md)** — components and the invariant-clause ownership map.
- **[docs/0004-build-brief.md](./docs/0004-build-brief.md)** — the original build plan.

## Contributing

Contributions are welcome — please read **[CONTRIBUTING.md](./CONTRIBUTING.md)** for the dev setup,
how the safety gate works, and the PR process, and **[GROUNDING.md](./GROUNDING.md)** for the
invariants any change is held against. By participating you agree to the
[Code of Conduct](./CODE_OF_CONDUCT.md). To report a vulnerability, see [SECURITY.md](./SECURITY.md).

## License

[MIT](./LICENSE) © Madhav Haldia
