//go:build ignore

// Command gen regenerates internal/buildinfo/versions.json from the canonical
// repo-root copy, so the single source of truth lives in ONE editable place (the
// root versions.json) while go:embed still has a sibling file to embed (it cannot
// reach a parent directory).
//
// Run it via `go generate ./internal/buildinfo/...` (the directive lives in
// buildinfo.go); go generate runs it with the working directory set to this
// package dir. We locate the canonical copy by walking up to go.mod — the same
// way drift_test.go finds the repo root — rather than hardcoding the depth. The
// copy is byte-for-byte: the root file's exact bytes are written here, so an
// already-in-sync tree stays `git status` clean after a generate. drift_test.go
// fails CI if the two ever diverge.
//
// This file is excluded from the normal build by the `ignore` build tag; it is
// only ever compiled/run by `go run` under go:generate.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// dst is the package-local embedded copy, written relative to the package dir
// (go generate's working directory).
const dst = "versions.json"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "buildinfo/gen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	src := filepath.Join(root, "versions.json")
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read canonical %s: %w", src, err)
	}
	// Preserve the root file's mode bits on the regenerated copy when possible,
	// falling back to a sane 0o644 for a fresh write.
	mode := os.FileMode(0o644)
	if fi, statErr := os.Stat(src); statErr == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		return fmt.Errorf("write embedded %s: %w", dst, err)
	}
	return nil
}

// repoRoot walks up from the working directory until it finds go.mod, mirroring
// drift_test.go's repoRoot so the generator and the guard agree on "the root".
func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate go.mod walking up from %s", wd)
		}
		dir = parent
	}
}
