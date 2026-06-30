# Contributing to mad-substrate

Thanks for your interest! mad-substrate is a safety-critical substrate, so the bar for changes is
correctness first. This guide covers the dev setup, the checks your change must pass, and the PR
process.

Before anything else, read **[GROUNDING.md](./GROUNDING.md)** — the 13 invariants. They are binding:
if a change can't be reconciled with them, the change loses, not the invariant.

## Dev setup

Requirements:

- **Go 1.26.4+** (the pin is in `go.mod`; the build is **cgo-free**)
- **git ≥ 2.38** (the integrator uses `git merge-tree --write-tree`)
- For the **container-grain** tests only: a Linux container runtime — Apple `container` on macOS
  (these tests *skip with a reason* when no runtime is present, so they're optional)

```sh
git clone https://github.com/madhavhaldia/mad-substrate
cd mad-substrate
make build          # cgo-free binary -> dist/mad-substrate
make test           # full suite under the race detector
make conform        # the executable safety gate (must be GREEN)
```

## Checks your change must pass

CI runs these on every PR; run them locally first:

```sh
gofmt -l .                       # must print nothing (formatting)
go vet ./...                     # must be clean
CGO_ENABLED=0 go build ./...     # the shipped binary is cgo-free — this MUST build
make test                        # CGO_ENABLED=1 go test ./... -race
make conform                     # exit 0 = GREEN
```

Notes:

- **cgo-free is non-negotiable for the binary.** `CGO_ENABLED=0 go build ./cmd/mad-substrate` must
  succeed. The race detector (`make test`) needs cgo, which is fine — that's the test build, not the
  ship build.
- **The JSON-RPC registry is frozen.** Adding, renaming, or removing a daemon method is a
  re-review-gated change to the contract — discuss it in an issue first.
- **Tests are the constitution's teeth.** The lease ledger, integrator, singular gate, and the
  `conform` safety checks are correctness-critical; new safety behavior needs hand-authored tests,
  and every negative/safety check needs a **non-vacuous control** that injects the violation and
  proves the check fails (a check that's green without proving its clause is the cardinal defect).
- **Container-grain rows** in `conform` skip on a runtime-less host. If your change affects the
  container grain, run the full gate on a host with the runtime.

### The cooperative layer

The cooperative layer is **native Go**, built into the single binary as the `mad-substrate mcp` and
`mad-substrate hook <event>` subcommands (`internal/mcp`, `internal/coophook`, `internal/coopclient`,
auto-wired into `mad-substrate launch` by `internal/coopwiring`). It is **advisory and fail-soft** (any
daemon-layer failure falls back to allowing the operation; it must never make a governed session
more fragile than a bare one) and is covered by the normal `make test` sweep — no separate
toolchain.

## Pull request process

1. **Open an issue first** for anything non-trivial — especially anything touching the daemon
   contract, the invariants, or a safety check — so the design can be agreed before code.
2. Fork, branch (`feature/...` or `fix/...`), and make focused commits.
3. Ensure all the checks above pass locally.
4. Open a PR against the default branch with a clear description: what changed, why, and how it's
   verified. Reference the issue and any relevant invariant (e.g. "Inv 7").
5. CI must be green and a maintainer must review. Safety-relevant changes get an adversarial review.

Keep PRs scoped — one concern per PR. Match the surrounding code's style, comment density, and
naming; favor clarity over cleverness in this codebase.

## How the maintainers integrate (context, not a requirement)

mad-substrate develops *itself* through its own governed loop (`mad-substrate trunk submit/promote` onto a
mediated trunk — see [docs/0005](./docs/0005-governed-trunk-loop.md)). **You do not need any of
that to contribute** — a normal GitHub fork-and-PR is all that's expected. The self-hosting loop is
how maintainers land reviewed changes; it's a nice thing to read about, not a contributor
prerequisite.

## Reporting bugs & security issues

- Functional bugs / feature ideas: open a GitHub issue (templates provided).
- **Security vulnerabilities: do NOT open a public issue** — see [SECURITY.md](./SECURITY.md).

By contributing, you agree your contributions are licensed under the project's [MIT License](./LICENSE).
