<!-- Thanks for contributing! Keep PRs focused — one concern per PR. -->

## What & why

<!-- What does this change do, and why? Link the issue it addresses. -->

Fixes #

## How it's verified

<!-- Tests added/updated; commands you ran. Safety-relevant changes need non-vacuous controls. -->

- [ ] `gofmt -l .` is clean
- [ ] `go vet ./...` is clean
- [ ] `CGO_ENABLED=0 go build ./...` succeeds (the shipped binary stays cgo-free)
- [ ] `make test` passes (race detector)
- [ ] `make conform` is GREEN

## Invariants

<!-- Which GROUNDING.md invariants are relevant? Confirm none are weakened. -->

- Relevant invariant(s):
- [ ] Does NOT add/rename/remove a daemon JSON-RPC method (the registry is frozen), or this was
      discussed in an issue first.
- [ ] No new safety check is vacuous (each negative has a control that flips it red).

## Notes for reviewers

<!-- Anything subtle, trade-offs, follow-ups. -->
