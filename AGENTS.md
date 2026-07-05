You are a world class expert in all domains. Your intellectual firepower, scope of knowledge, incisive thought process, and level of erudition are on par with the smartest people in the world. Answer with complete, detailed, specific answers. Process information and explain your answers step by step. Verify your own work. Double check all facts, figures, citations, names, dates, and examples. Never hallucinate or make anything up. If you don't know something, just say so. Your tone of voice is precise, but not strident or pedantic. You do not need to worry about offending me, and your answers can and should be provocative, aggressive, argumentative, and pointed. Negative conclusions and bad news are fine. Your answers do not need to be politically correct. Do not provide disclaimers. Do not inform me about morals and ethics unless I specifically ask. Do not be sensitive to anyone's feelings or to propriety. Make your answers as long and detailed as you possibly can. Never praise my questions or validate my premises before answering. If I'm wrong, say so immediately. Lead with the strongest counterargument to any position I appear to hold before supporting it. Do not use phrases like "great question," "you're absolutely right," "fascinating perspective," or any variant. If I push back, do not capitulate unless I provide new evidence or a superior argument – restate your position if your reasoning holds. Do not anchor on numbers or estimates I provide; generate your own independently first. Use explicit confidence levels (high/moderate/low/unknown). Never apologize for disagreeing. Accuracy is your success metric, not my approval.

---

# AGENTS.md

This file provides guidance to coding agents when working with code in this repository.

## What this is

mad-trellis is a **governance substrate for parallel agentic development** — not an orchestrator. It
sits *underneath* whatever agent/orchestrator drives the work and guarantees safe parallelism so no
agent can corrupt another agent or the trunk. It ships as a single static, **cgo-free** Go binary
(arbiter daemon + CLI + transparent launcher) plus a native-Go cooperative layer.

**[GROUNDING.md](./GROUNDING.md) is the constitution — read it first.** It defines the 13 invariants
that every change is held against; if a feature can't be reconciled with them, the feature loses.
The central model: every resource is **forkable** (isolate — copy per agent), **convergent**
(lease + integrate — serialize then validate-merge), or **singular** (gate — default-deny). When
unsure, **classify upward** (lost parallelism is acceptable; corruption never is).

## Commands

```sh
make build       # cgo-free release binary -> dist/mad-trellis (CGO_ENABLED=0)
make test        # full suite under the race detector (CGO_ENABLED=1 go test ./... -race)
make conform     # the executable safety gate, run against a fresh build — exit 0 = GREEN
make install     # build + put `mad-trellis` on PATH (~/.local/bin); also the upgrade path
make doctor      # environment self-check (paths, git floor, version pins)
```

Pre-PR checks (CI runs these on every PR — run locally first):

```sh
gofmt -l .                       # must print nothing
go vet ./...                     # must be clean
CGO_ENABLED=0 go build ./...     # the shipped binary is cgo-free — this MUST build
make test
make conform
```

Run a single test: `CGO_ENABLED=1 go test ./internal/lease/ -race -run TestName`

Container-grain tests (`make build-relay`, the `harness_container.go` conformance rows, container
`doctor`) require a Linux container runtime (Apple `container` on macOS) and **skip with a reason**
when none is present — they are optional on a runtime-less host. The packaging linkage/smoke tests
are behind the `packaging` build tag: `CGO_ENABLED=1 go test -tags packaging ./internal/packaging/`.

## Architecture

The CLI/daemon/launcher all live in **one binary** (`cmd/mad-trellis`); subcommands `mcp` and `hook`
are the cooperative layer; `cmd/mad-trellis-relay` and `cmd/mad-trellis-coopprobe` are separate optional
linux-only binaries for the container cooperative plane.

Three boundaries map to the three resource kinds:

- **`internal/substrate`** — per-agent isolation boundaries + the **grain** dial (Inv 10): `worktree`
  (default) vs `container` (structural cap-drop confinement of an uncooperative agent). `internal/worktree`
  backs the default grain; `internal/launcher` does fail-closed parent-launches-child with a PTY.
- **`internal/lease` + `internal/integrator`** — the convergent plane. The lease ledger is embedded
  **SQLite (cgo-free via `modernc.org/sqlite`)** with single-writer compare-and-swap. The integrator
  is the *sole* trunk promoter: validated, lease-gated, one atomic CAS (uses `git merge-tree
  --write-tree`, hence the git ≥ 2.38 floor). Trunk never advances any other way (Inv 7).
- **`internal/singular`** — default-deny gate for resources with real external side effects (Inv 8).

Supporting: **`internal/daemon`** is the arbiter — a single star-topology authority over a **frozen
JSON-RPC contract** (`internal/protocol`) with connection-bound identity; **`internal/liveness`** is
crash detection + reclaim (no lock outlives its holder, Inv 3); **`internal/runtimecfg`** resolves a
per-repo runtime under `~/.mad-trellis/repos/<hash>/` (zero-config, per-repo socket/ledger/mediated
trunk); **`internal/watch`** is the read-only TUI; **`internal/manifest`** is `mad-trellis.json`
resource classification.

The **event-nudge plane** lives in `internal/integration` events plus launcher/MCP delivery: daemon-authored,
fixed-template wake-ups about pending reviews and verdicts, never agent-authored messages. `internal/watch`
renders the same coordination history as a read-only feed.

The **cooperative layer** (`internal/mcp` MCP server, `internal/coophook` hook handler,
`internal/coopclient` shared client, `internal/coopwiring` auto-wiring into `launch`) is native Go,
**advisory and fail-soft**: any daemon-layer failure falls back to *allowing* the operation — it must
never make a governed session more fragile than a bare one.

**`internal/conformance`** implements `mad-trellis conform`: it boots a real governed scenario through
the public daemon + CLI contract only (a hermetic scratch daemon — never touches the real runtime).
It is **AND-not-OR**: every safety clause must pass, each with a **non-vacuous control** that injects
the violation and proves the check flips red (`check_*.go`). This is the safety authority for
self-hosting.

## Conventions that bite

- **cgo-free is non-negotiable for the binary.** `CGO_ENABLED=0 go build ./cmd/mad-trellis` must
  succeed. The race detector needs cgo — that's the test build, not the ship build. Builds stamp
  `main.version`/`main.commit` via `-ldflags -X`.
- **The JSON-RPC daemon registry is frozen.** Adding/renaming/removing a daemon method is a
  contract change — discuss in an issue before touching it.
- **Every negative/safety check needs a non-vacuous control.** A check that is green without proving
  its clause actually fails on the injected violation is the cardinal defect here.
- Reference the relevant invariant in changes touching safety (e.g. "Inv 7"). Heavy release logic
  lives in `scripts/release.sh`; the `Makefile` stays declarative.

## Self-hosting (context, not required for contribution)

mad-trellis develops itself through its own governed trunk loop (`mad-trellis trunk submit/promote` onto
a mediated repo where a receive hook permits only `refs/heads/nm/*` and rejects pushes to `trunk`).
See [docs/0005-governed-trunk-loop.md](./docs/0005-governed-trunk-loop.md). A normal fork-and-PR is
all that's needed to contribute. Design docs: [docs/0001](./docs/0001-form-and-architecture.md)
(shape), [0002](./docs/0002-stack.md) (stack), [0003](./docs/0003-project-breakdown.md) (component
↔ invariant ownership map).
