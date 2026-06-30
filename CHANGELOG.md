# Changelog

All notable changes to mad-substrate are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-06-28

First release. mad-substrate is a governance substrate for parallel agentic
development — it sits underneath whatever agent or orchestrator drives the work and
guarantees safe parallelism so no agent can corrupt another agent or the trunk.

### Added

- Single static, cgo-free Go binary (`mad-substrate`) bundling the arbiter daemon,
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
- Read-only TUI: `mad-substrate watch`.
- Short `ms` alias for the binary: `make install` symlinks it alongside
  `mad-substrate`, and `mad-substrate alias` creates it for release-binary installs.
- Resource classification via `mad-substrate.json` (forkable / convergent / singular).
- `mad-substrate conform`: the executable, AND-not-OR safety gate that boots a real
  governed scenario through the public daemon + CLI contract, each safety clause
  backed by a non-vacuous control.
- Reproducible release pipeline shipping darwin/arm64, linux/amd64, and linux/arm64
  binaries with checksums.

[Unreleased]: https://github.com/madhavhaldia/mad-substrate/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/madhavhaldia/mad-substrate/releases/tag/v0.1.0
