# mad-trellis release / packaging targets (project 10b distribution-packaging).
#
# Thin by design: heavy logic lives in scripts/ so the targets stay declarative
# and the reproducible pipeline (scripts/release.sh) is the single source of truth.
# All Go builds are cgo-free; the race-tagged checks (test/linkage/smoke) need cgo.

# VERSION/COMMIT are stamped into the binary via -ldflags -X main.{version,commit}.
# git describe gives a release-meaningful version; the fallbacks keep an untagged
# or non-git tree building.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# GO_MIN — the minimum Go toolchain, read straight from the `go` directive in go.mod
# (the single source of truth; no second copy to drift). The go-preflight target
# below uses it to fail with an actionable message BEFORE the compiler does.
GO_MIN := $(shell awk '/^go /{print $$2; exit}' go.mod)

# install prefix for `make install` (local laptop use; no root needed by default).
# ~/.local/bin is on a typical user PATH. Override: make install PREFIX=/usr/local
# (BINDIR defaults to $(PREFIX)/bin), or point BINDIR straight at any dir on PATH.
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

# ALIAS is the short convenience name symlinked alongside the binary by
# `make install` (so you can type `ms` instead of `mad-trellis`). Disable it
# with `make install ALIAS=`; rename it with e.g. `make install ALIAS=sub`.
ALIAS ?= ms

.PHONY: build build-linux build-relay coop-assets go-preflight install uninstall test conform doctor linkage smoke release clean

# go-preflight — fail the build/install path with an ACTIONABLE message when the
# installed Go is older than the go.mod toolchain (GO_MIN), instead of letting the
# compiler emit a cryptic "note: module requires Go X" error. `go env GOVERSION`
# reports the active toolchain (e.g. go1.26.4); sort -V does the numeric compare.
go-preflight:
	@command -v go >/dev/null 2>&1 || { \
		echo "mad-trellis needs Go >= $(GO_MIN), but no 'go' was found on your PATH — see https://go.dev/dl"; \
		exit 1; \
	}
	@have=$$(go env GOVERSION 2>/dev/null | sed 's/^go//'); \
	need=$(GO_MIN); \
	if [ "$$(printf '%s\n%s\n' "$$need" "$$have" | sort -V | head -n1)" != "$$need" ]; then \
		echo "mad-trellis needs Go >= $$need; found $$have — see https://go.dev/dl"; \
		exit 1; \
	fi

# COOP_ARCHES — the linux arches whose cooperative-plane payloads are EMBEDDED into
# the shipped darwin binary so the container grain's cooperative plane is ON BY
# DEFAULT (no host asset to configure). arm64 is the Apple `container` arch on Apple
# silicon; amd64 is built too (a trivial pure-Go cross-compile) to cover an x86 linux
# runtime.
COOP_ARCHES := arm64 amd64
COOP_ASSETS  := internal/coopembed/assets

# coop-assets — cross-build the static linux relay (cmd/mad-trellis-relay) + linux
# mad-trellis (cmd/mad-trellis) PAYLOADS into the embed dir for each COOP_ARCH.
# CRITICAL: the embedded linux mad-trellis is the PAYLOAD and is built WITHOUT -tags
# coopembed — it must NOT recursively embed itself; only the shipped darwin binary
# (the `build` target) is tagged coopembed. cgo-free pure-Go cross-compiles need no C
# toolchain, so this runs on a darwin host. The .gitignore keeps these artifacts out
# of git (only .gitkeep is committed).
coop-assets:
	mkdir -p $(COOP_ASSETS)
	@for arch in $(COOP_ARCHES); do \
		echo "==> coop-assets: linux/$$arch relay + mad-trellis payload"; \
		GOOS=linux GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(COOP_ASSETS)/mad-trellis-relay-linux-$$arch ./cmd/mad-trellis-relay || exit 1; \
		GOOS=linux GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o $(COOP_ASSETS)/mad-trellis-linux-$$arch ./cmd/mad-trellis || exit 1; \
	done

# build — the shippable, cgo-free binary, with the cooperative-plane linux payloads
# EMBEDDED (-tags coopembed, via the coop-assets prereq) so the container grain is
# cooperative by default with no host asset to configure. Still cgo-free
# (CGO_ENABLED=0). -trimpath + -s -w keep it reproducible and stripped; -X stamps the
# release identity onto main.version/main.commit.
build: go-preflight coop-assets
	CGO_ENABLED=0 go build -trimpath -tags coopembed -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o dist/mad-trellis ./cmd/mad-trellis

# build-linux — cross-build the cgo-free linux binaries for both amd64 and arm64.
# Cross-compiling pure-Go (CGO_ENABLED=0) needs NO C toolchain, so this runs on a
# darwin host. Same -trimpath + -s -w + -X stamping as `build` for reproducible,
# stripped artifacts. These are build-only on a non-linux host — they cannot be run
# or smoked here; linux smoke/conform require a linux CI host.
build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o dist/mad-trellis-linux-amd64 ./cmd/mad-trellis
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o dist/mad-trellis-linux-arm64 ./cmd/mad-trellis

# build-relay — cross-build the cooperative-plane in-container relay + probe (#2)
# for both linux arches. The relay is the static, cgo-free linux binary the launcher
# stages into a container and execs as the exec-stdio tunnel; point
# MAD_CONTAINER_RELAY at the arch matching your container runtime (Apple
# `container` on macOS = arm64) to opt the cooperative plane in. mad-trellis-coopprobe
# is an in-container diagnostic/e2e client. These are separate binaries — NOT linked
# into the main daemon — so the shipped `mad-trellis` stays unchanged.
build-relay:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/mad-trellis-relay-linux-amd64 ./cmd/mad-trellis-relay
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/mad-trellis-relay-linux-arm64 ./cmd/mad-trellis-relay
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/mad-trellis-coopprobe-linux-amd64 ./cmd/mad-trellis-coopprobe
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/mad-trellis-coopprobe-linux-arm64 ./cmd/mad-trellis-coopprobe

# install — build the cgo-free binary and put `mad-trellis` on your PATH for local use
# (no publishing needed). Installs to $(BINDIR) (default ~/.local/bin). The daemon,
# CLI, spawn/launch/integrate/trunk/watch/doctor AND the native cooperative layer
# (`mad-trellis mcp` / `mad-trellis hook`) all ship in this one binary; the container
# relay (`make build-relay`) is a separate, optional add-on.
install: build
	mkdir -p "$(BINDIR)"
	install -m 0755 dist/mad-trellis "$(BINDIR)/mad-trellis"
	@echo "installed: $(BINDIR)/mad-trellis ($(VERSION))"
	@if [ -n "$(ALIAS)" ]; then \
		ln -sf mad-trellis "$(BINDIR)/$(ALIAS)"; \
		echo "alias installed: $(BINDIR)/$(ALIAS) -> mad-trellis (disable with ALIAS=)"; \
	fi
	@case ":$$PATH:" in *":$(BINDIR):"*) : ;; *) echo "NOTE: $(BINDIR) is not on your PATH — add it: export PATH=\"$(BINDIR):$$PATH\"";; esac

# uninstall — remove the installed binary (leaves ~/.mad-trellis runtime state alone).
uninstall:
	rm -f "$(BINDIR)/mad-trellis"
	@if [ -n "$(ALIAS)" ] && [ -L "$(BINDIR)/$(ALIAS)" ]; then rm -f "$(BINDIR)/$(ALIAS)"; echo "removed: $(BINDIR)/$(ALIAS)"; fi
	@echo "removed: $(BINDIR)/mad-trellis"

# test — the full suite under the race detector (race REQUIRES cgo).
test:
	CGO_ENABLED=1 go test ./... -race

# conform — the 10a safety gate, run against the freshly built release binary.
conform: build
	./dist/mad-trellis conform

# doctor — the environment self-check, run against the freshly built release binary.
doctor: build
	./dist/mad-trellis doctor

# linkage — the cgo carve-out guard (Stage D writes these tagged tests; this target
# may run before they exist, which is fine — go reports "no tests to run").
linkage:
	CGO_ENABLED=1 go test -tags packaging ./internal/packaging/ -run Linkage

# smoke — the hermetic packaging smoke (Stage D writes these tagged tests).
smoke:
	CGO_ENABLED=1 go test -tags packaging ./internal/packaging/ -run Smoke

# release — the reproducible pipeline: refuses on a dirty tree or a RED 10a gate,
# then builds + guards + checksums into dist/.
release:
	bash scripts/release.sh

# clean — drop all release artifacts (dist/) and the cross-built embed payloads
# (the .gitignored binaries under internal/coopembed/assets/; .gitkeep is preserved).
clean:
	rm -rf dist
	find $(COOP_ASSETS) -type f ! -name .gitkeep -delete
