//go:build !coopembed

// Package coopembed optionally carries the static linux cooperative-plane payloads
// — the in-container relay (cmd/mad-trellis-relay) and the linux mad-trellis binary —
// EMBEDDED into the shipped darwin binary, so the container-grain cooperative plane
// is ON BY DEFAULT with no host asset to point at (Inv 10; #2).
//
// BUILD-TAG STUB/REAL SPLIT. This file is the STUB face (//go:build !coopembed): a
// plain `go build` (no -tags coopembed) embeds NOTHING — keeping the default
// cgo-free build asset-free, small, and the critical CI invariant
// (`CGO_ENABLED=0 go build ./...` with NO tag) green even on a tree whose
// assets/ dir holds only .gitkeep. The REAL face (coopembed_embed.go,
// //go:build coopembed) embeds internal/coopembed/assets/* and is selected only by
// the Makefile `build` target, which cross-builds those linux binaries first.
//
// FAIL-SOFT CONTRACT: every accessor returns "absent" rather than failing, because
// the cooperative plane is a coordination nicety, never a safety boundary — a build
// without assets simply runs the container agent confined without the plane.
package coopembed

// Available reports whether the embed face is compiled into this build. Always false
// in the stub (untagged) build.
func Available() bool { return false }

// RelayBytes returns the embedded static linux relay binary for goarch, or
// (nil, false) when absent. Always (nil, false) in the stub build.
func RelayBytes(goarch string) ([]byte, bool) { return nil, false }

// MadTrellisBytes returns the embedded static linux mad-trellis binary for goarch, or
// (nil, false) when absent. Always (nil, false) in the stub build.
func MadTrellisBytes(goarch string) ([]byte, bool) { return nil, false }
