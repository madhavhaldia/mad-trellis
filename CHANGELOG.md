# Changelog

All notable changes to mad-trellis are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `mad-trellis integrator stop`: stop the running integrator on this trunk and
  free its singleton presence lease. It finds the integrator via a pidfile the
  MCP server writes beside the ledger, verifies the pid is really an integrator
  (a reused pid is never signalled), and sends the SIGTERM the server handles by
  releasing its lease; `--force` escalates to SIGKILL.

### Fixed

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

[Unreleased]: https://github.com/madhavhaldia/mad-trellis/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/madhavhaldia/mad-trellis/releases/tag/v0.1.0
