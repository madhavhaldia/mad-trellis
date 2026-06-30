# 0002 — Tech Stack

*Status: accepted. Locked early on purpose: the stack is the expensive-to-reverse foundation.*
*The manifest, tier boundaries, and workflows are deliberately NOT locked here — those are*
*emergent and can only be settled by running the harness against real tasks.*

---

## Decision

| Layer | Choice | Rationale |
|-------|--------|-----------|
| **Core engine / CLI / daemon** | **Go** | Native language of this category (Docker, k8s, containerd, Tailscale, `gh`). Process/PTY/FS/container control are first-class. Single static binary. Best **agent-iteration loop** for a systems daemon: fast compiles, explicit errors, `gofmt`/`vet`, one-obvious-way → low ambiguity, tight self-correction. |
| **Lease ledger** | **Embedded SQLite** | Atomic transactions (Inv. 2) + durable (Inv. 3) with **zero external infra** — mad-substrate stays standalone. Redis only if we ever need multi-host; not now. |
| **Cooperative layer** (Claude Code first) | **Native Go** (in the single binary) | The `mad-substrate mcp` MCP server + `mad-substrate hook <event>` handler — thin: translate agent calls → daemon API. No separate Node/TS toolchain. |
| **Daemon protocol** (CLI/cooperative layer ↔ daemon) | **Local Unix-socket JSON-RPC** | Simple, stable, language-agnostic. Keeps the agent-facing MCP protocol decoupled from the daemon's own API (Inv. 10). |
| **Isolation substrate** | **Conduct existing tools** — `git worktree`, Docker, a VM tool (Lima/Tart) | Never reimplement (doc 0001). macOS + Apple Silicon first. **v1 grain = worktree**; grain is a dial widened later. |
| **Distribution** | **Single Go binary** | `brew install` later. No runtime to ship. |

## Why Go for a vibe-coded core

This is written by a coding agent, not by hand, so the selection criterion is **which language an
agent builds this workload best in** — not the author's proficiency.

- **Tight loop:** sub-second compiles + explicit errors → fast, unambiguous self-correction. Rust's
  borrow-checker lengthens that loop; TS pushes failures to runtime, worst for a correctness-critical
  daemon.
- **Low ambiguity:** `gofmt` + `vet` + small type system + "boring" culture → one idiomatic path.
  Agents wander in Rust's type-level cleverness and TS's ecosystem churn (ESM/CJS, async coloring).
- **Category-native libs:** git, PTY, Docker SDK, process control are first-class.

Go's category-native libs (process, PTY, git, sockets) carry the whole stack — including the
cooperative MCP/hook surface, which is now native Go in the single binary rather than a separate TS
package. One language, one binary: less ecosystem churn (ESM/CJS, async coloring), one idiomatic
path.

## Split

- **Go** — everything: the correctness-critical core (substrate manager, lease service, integrator,
  daemon, CLI) AND the thin per-host cooperative layer (hooks + MCP), all in the single binary.
- **SQLite** — the durable lease ledger, embedded in the Go binary.

## Pinned versions — verified 2026-06-19

Verified against official sources (go.dev/dl, proxy.golang.org, GitHub releases), not from memory.
Re-verify before a fresh `go mod tidy`. ⚠ = moved past what older tooling/assumptions expect.

> The cooperative layer is now **native Go** inside the single binary (the `mad-substrate mcp` /
> `mad-substrate hook` subcommands) — there is no separate Node / pnpm / TypeScript toolchain.

### Toolchains
| Tool | Version | Notes |
|------|---------|-------|
| Go | **go1.26.4** | `go.mod` directive `go 1.26.4`. 1.27 is RC — stay on stable. |

### Go modules (the core)
| Module | Version | Enters in |
|--------|---------|-----------|
| `modernc.org/sqlite` | **v1.52.0** | **v1** — lease ledger. Pure-Go, cgo-free → keeps single static binary. |
| `github.com/spf13/cobra` | **v1.10.2** | **v1** — CLI. Brings `spf13/pflag` (cobra's own flag library; `cmd.Flags()` returns `*pflag.FlagSet`) — part of the cobra surface, not a separate third-party dep (chafe: P3 promoted it indirect→direct for the no-goals flag-audit test, go.sum unchanged). |
| `github.com/creack/pty` | **v1.1.24** | substrate/launcher — agent PTY |
| `golang.org/x/term` | **v0.44.0** | launcher — parent-terminal raw mode (chafe C1). `golang.org/x/*` extended-stdlib pkgs are allowed even in the v1 set. |
| `github.com/go-git/go-git/v5` | **v5.19.1** | ⚠ does NOT support LINKED worktrees (chafe C3) → the worktree grain CONDUCTS the `git worktree` CLI (on-thesis: conduct, don't reimplement). Reserved for non-worktree git ops. |
| `github.com/charmbracelet/bubbletea` | **v1.3.10** | `watch` view |
| `github.com/charmbracelet/lipgloss` | **v1.1.0** | `watch` view |
| `github.com/docker/docker` | **v28.5.2+incompatible** | container grain (`+incompatible` is expected here) |

Daemon protocol: hand-rolled minimal **JSON-RPC 2.0 over Unix socket on stdlib `net`** — no external
dep. Tests: stdlib **`testing`**.

### Cooperative layer (Claude Code first)
Native Go in the single binary — the `mad-substrate mcp` MCP server + the `mad-substrate hook <event>`
handler (`internal/mcp`, `internal/coophook`, `internal/coopclient`, auto-wired by
`internal/coopwiring`). It speaks the agent-facing MCP dialect over the frozen daemon JSON-RPC and
adds no third-party dependency — no Node / pnpm / TypeScript / `@modelcontextprotocol/sdk`.

### macOS substrate tools (later isolation grains)
| Tool | Version | Notes |
|------|---------|-------|
| Colima | **v0.10.3** | container grain |
| Lima | **v2.1.3** | ⚠ take this exact version — CVE-2026-53657 fix; 2.x major |
| Tart | **2.32.1** | VM grain; tag has no `v` prefix |

### v1 dependency set (lease service + CLI skeleton)
**Go 1.26.4 + `modernc.org/sqlite` v1.52.0 + `spf13/cobra` v1.10.2** (+ `spf13/pflag`, cobra's own
flag library — counted as part of cobra, like `x/sys` is part of `x/term`) + the `golang.org/x/*`
extended-stdlib packages as needed (e.g. `golang.org/x/term` for the launcher, chafe C1). No
third-party module beyond these in v1; every other module above enters only in its later phase.

## Explicitly NOT locked (emergent through real use)

- The manifest schema and its default-classification policy.
- Where tier boundaries fall (what's forkable vs convergent vs singular in practice).
- Integration-view behavior (continuous vs gated merge, conflict surfacing).

These land through iteration against real tasks, not up-front design.
