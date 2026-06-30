#!/usr/bin/env bash
# scripts/release.sh — the reproducible mad-substrate release pipeline (project 10b).
#
# The pipeline is ORDERED so that the 10a conformance gate is load-bearing: a RED
# gate aborts the run BEFORE any artifact or checksum is produced. We never ship a
# binary whose safety properties have not been re-proven against THIS build.
#
# Order: clean-tree guard -> build (darwin host) -> THE GATE -> packaging guards
# -> linux cross-build -> checksums -> manifest. Any failing step aborts (set -e).
# The cooperative layer is native Go inside the binary now — there is no separate
# adapter bundle step.
#
# CROSS-GOOS: the darwin host binary is built, GATED (10a conform), and packaging-
# guarded (linkage + smoke) as the load-bearing release gate. The linux binaries are
# cross-compiled cgo-free (no C toolchain needed) and are BUILD-ONLY here: they
# cannot be run, smoked, or conform-gated on a darwin host. Their linux smoke/conform
# must run on a linux CI host. We still checksum + ship them as artifacts.
set -euo pipefail

# Deterministic toolchain location first on PATH (Homebrew Go on this host).
export PATH="/opt/homebrew/bin:$PATH"

# Anchor at the repo root regardless of how the script was invoked.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO"

# VERSION/COMMIT mirror the Makefile so a `make build` and a `release` agree.
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"

echo "==> mad-substrate release  version=$VERSION  commit=$COMMIT"

# --- Step 1: clean working tree -------------------------------------------------
# A reproducible release must build from a committed state. ALLOW_DIRTY=1 is the
# explicit escape hatch for local dry-runs only (it taints VERSION with -dirty).
if [ -n "$(git status --porcelain)" ] && [ "${ALLOW_DIRTY:-}" != 1 ]; then
	echo "release refuses: working tree is dirty (commit or stash first, or set ALLOW_DIRTY=1 for a local dry-run)" >&2
	exit 1
fi

# --- Step 2: build the cgo-free release binary (darwin host) --------------------
echo "==> [2/7] build  ->  dist/mad-substrate (cgo-free, trimpath, stripped)"
make build

# --- Step 3: THE GATE (load-bearing) -------------------------------------------
# Run the 10a conformance gate against the binary we just built. If it is RED we
# abort HERE, before producing any artifact or checksum — a red gate must never
# yield a shippable bundle.
echo "==> [3/7] conformance gate (10a) — the load-bearing safety check"
if ! ./dist/mad-substrate conform; then
	echo "release REFUSED: the 10a conformance gate is RED — not safe to ship" >&2
	exit 1
fi

# --- Step 4: packaging guards --------------------------------------------------
# The cgo carve-out (the binary stays statically linked / no surprise C deps) and
# the hermetic packaging smoke. A failure here also aborts before any artifact.
# These run against the HOST (darwin) binary; the cross-GOOS linkage scan asserts
# linux too when run on a linux CI host.
echo "==> [4/7] packaging guards (linkage + smoke)"
make linkage smoke

# --- Step 5: linux cross-build (build-only on a darwin host) --------------------
# Cross-compile the cgo-free linux binaries (amd64 + arm64). No C toolchain is
# needed for pure-Go cross-compilation. These CANNOT be run, smoked, or conform-
# gated here — linux smoke/conform require a linux CI host — but they ARE shipped
# artifacts, so we build and checksum them. The darwin gate above remains the
# load-bearing release gate.
echo "==> [5/7] linux cross-build  ->  dist/mad-substrate-linux-{amd64,arm64} (build-only on darwin)"
make build-linux

# --- Step 6: checksums ---------------------------------------------------------
# Checksum every shipped artifact: the darwin host binary and the cross-built
# linux binaries. The cooperative layer ships inside these binaries (native Go),
# so there is no separate adapter artifact to checksum.
echo "==> [6/7] checksums  ->  dist/SHA256SUMS"
( cd dist && shasum -a 256 mad-substrate mad-substrate-linux-amd64 mad-substrate-linux-arm64 > SHA256SUMS )

# --- Step 7: artifact manifest -------------------------------------------------
echo "==> [7/7] release artifacts in dist/:"
# ls -l with human sizes; portable across BSD (macOS) and GNU ls.
ls -lh dist/mad-substrate dist/mad-substrate-linux-amd64 dist/mad-substrate-linux-arm64 dist/SHA256SUMS
echo
echo "==> SHA256SUMS:"
cat dist/SHA256SUMS
echo
echo "==> release OK  (version=$VERSION)"
echo "==> note: linux binaries are build-only on this darwin host; run linux smoke/conform on a linux CI host before publishing."
