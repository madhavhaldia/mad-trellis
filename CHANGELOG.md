# Changelog

All notable changes to mad-trellis are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.1] - 2026-07-08

### Changed

- **Default short alias renamed `ms` -> `mt`** (the initials of mad-trellis —
  `ms` dated from the mad-substrate name): `make install` now symlinks `mt`,
  and `mad-trellis alias` defaults to `mt`. An existing `ms` symlink keeps
  working (it points at the binary) but is no longer installed or removed by
  make targets — delete it manually if unwanted, or keep any name with
  `make install ALIAS=<name>`.

## [0.2.0] - 2026-07-08

First release under the **mad-trellis** name (v0.1.0 shipped as mad-substrate).
The headline is the **event-nudge plane**: daemon-authored wake-ups that close
the integration review loop without polling — and without ever becoming an
agent-to-agent message channel.

### Added

- **Event plane for the integration review loop**: governed state transitions
  (a request created, claimed, or given a verdict) now produce small,
  daemon-authored, audience-scoped event rows (`integration.events`). Events are
  payload-free wake-ups (id / kind / branch / timestamp only) — the durable
  integration rows remain the source of truth. See
  [docs/0007-event-nudges.md](./docs/0007-event-nudges.md).
- **Nudge delivery**: `mad-trellis launch` and `integrator run` sessions receive
  fixed-template nudge lines injected into the agent's PTY, with a politeness
  guard that defers while user input is fresh; sessions not launched through the
  PTY wrapper get the same nudges piggybacked onto MCP tool results.
  `MAD_NUDGES=off` disables delivery (never the state machine).
- **Integrator session support**: `mad-trellis integrator run` wraps an
  integrator agent with the same PTY plumbing plus integrator-audience nudges
  and a session keepalive; the MCP layer gained role-correct guidance and a
  no-integrator advisory for builders whose requests have no one to review them.
- `mad-trellis integrator stop`: stop the running integrator on this trunk and
  free its singleton presence lease. It finds the integrator via a pidfile the
  MCP server writes beside the ledger, verifies the pid is really an integrator
  (a reused pid is never signalled), and sends the SIGTERM the server handles by
  releasing its lease; `--force` escalates to SIGKILL.
- **Conformance grew event-plane safety clauses** (with non-vacuous controls):
  events are payload-free (field set pinned), branch-audience events are
  isolated from strangers, no mesh/broadcast method exists on the contract, and
  the Inv 13 nudge carve-out is proven fixed-template and daemon-authored.
- `watch` redesigned around a height-budgeted, chat-shaped **coordination feed**
  rendering the same governed history the event plane produces (read-only, as
  ever).
- ADR 0008 (PROPOSED): the liveness redesign — suspension-aware death oracle,
  quarantine, and salvage-before-destroy
  ([docs/0008](./docs/0008-death-oracle-and-salvage.md)).

### Changed

- **Project renamed from mad-substrate to mad-trellis**: the binary, the Go
  module path (`github.com/madhavhaldia/mad-trellis`), the manifest filename
  (`mad-trellis.json`), and the per-repo runtime root (`~/.mad-trellis/`). The
  `ms` short alias is unchanged. Existing installs should `make install` (or
  reinstall) under the new name; per-repo runtimes are re-created on first
  launch.

### Fixed

- **Nudges are now actually submitted in burst-detecting TUIs (Codex)**:
  delivery used to be one PTY write of `body\r`, which paste-burst heuristics
  treat as pasted text — the nudge landed in the composer and was never sent.
  The body is now injected as an insert-text event (bracketed-paste aware,
  tracked from the child's own output) and the submit as a temporally isolated
  lone `\r` keypress, so delivery no longer depends on which agent sits behind
  the PTY (Inv 10).
- The integrator MCP server now traps **SIGHUP** (terminal-window close/hangup),
  so closing an integrator's terminal releases its presence lease immediately
  instead of stranding it until the 60s TTL lapsed (which made `integrator
  status` keep reporting "running" and `recover` reclaim nothing). Inv 3.
- The MCP stdio read loop is now cancellable: a signalled server actually exits
  instead of hanging on a blocked stdin read while only its lease was released.
- `mad-trellis launch` and `mad-trellis integrator start` now fail fast with an
  actionable message when run outside a git repository, instead of auto-starting
  a daemon and opening an integrator terminal before failing late on the boundary
  provision (`git worktree add: not a git repository`).

## [0.1.0] - 2026-06-28

First release. mad-trellis is a governance substrate for parallel agentic
development — it sits underneath whatever agent or orchestrator drives the work and
guarantees safe parallelism so no agent can corrupt another agent or the trunk.

### Added

- Single static, cgo-free Go binary (`mad-trellis`) bundling the arbiter daemon,
  the CLI, and the transparent launcher.
- Two isolation grains via the `MAD_GRAIN` dial: `worktree` (default; per-agent git
  worktree boundaries) and `container` (structural cap-drop confinement of an
  uncooperative agent on a Linux container runtime).
- Convergent plane: a single-writer SQLite lease ledger (cgo-free via
  `modernc.org/sqlite`) plus an integrator that is the sole trunk promoter
  (validated, lease-gated, one atomic CAS via `git merge-tree --write-tree`;
  requires git >= 2.38).
- Default-deny singular gate for resources with real external side effects.
- Cooperative layer (native Go, advisory and fail-soft): an MCP server exposing
  `mad_*` tools, a hook handler, and auto-wiring into `launch`, with an embedded
  Linux relay so the container grain's cooperative plane is on by default.
- Crash detection and lease reclaim so no lock outlives its holder.
- Read-only TUI: `mad-trellis watch`.
- Short `ms` alias for the binary: `make install` symlinks it alongside
  `mad-trellis`, and `mad-trellis alias` creates it for release-binary installs.
- Resource classification via `mad-trellis.json` (forkable / convergent / singular).
- `mad-trellis conform`: the executable, AND-not-OR safety gate that boots a real
  governed scenario through the public daemon + CLI contract, each safety clause
  backed by a non-vacuous control.
- Reproducible release pipeline shipping darwin/arm64, linux/amd64, and linux/arm64
  binaries with checksums.

[Unreleased]: https://github.com/madhavhaldia/mad-trellis/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/madhavhaldia/mad-trellis/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/madhavhaldia/mad-trellis/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/madhavhaldia/mad-trellis/releases/tag/v0.1.0
