// Package packaging holds the Stage-D packaging guards for project 10b
// (distribution-packaging): the cgo carve-out linkage test and the hermetic
// packaging smoke. The actual guards live in files carrying `//go:build
// packaging` (linkage_test.go, smoke_test.go), so they run ONLY under
// `go test -tags packaging ./internal/packaging/` (what the Makefile linkage and
// smoke targets invoke).
//
// This non-tagged file exists solely so the package is never "empty" to the
// untagged toolchain: without it, a plain `go test ./...` reports the package as
// "build constraints exclude all Go files" (a per-package setup FAILURE). With
// it, the untagged sweep cleanly reports "[no test files]" and the heavy,
// host-specific guards stay opt-in behind the build tag.
package packaging
