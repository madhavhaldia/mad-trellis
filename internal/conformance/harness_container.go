package conformance

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
)

// harness_container.go extends the hermetic Scratch so a CONFINEMENT check can
// request a CONTAINER-GRAIN boundary and drive an UNCOOPERATIVE agent INSIDE it.
// It reuses Stage 1/2: the daemon's substrate already selects the container grain
// off MAD_GRAIN (compose.go → substrate.New's dial), and the container grain
// provisions a container with ONLY the agent worktree bind-mounted at /work (plus the
// agent's own per-agent writable state + an ephemeral /tmp). The grain is now
// COOPERATIVE-by-default — default caps + a WRITABLE rootfs + the runtime's default
// network (egress), matching the worktree grain's host posture. HOST isolation is
// MOUNT-STRUCTURAL (only /work + the state dir are mounted) and holds regardless of
// mode. The full CONFINEMENT bundle (--cap-drop ALL + --read-only rootfs + network
// none) is the explicit OPT-IN the confinement tier requests via
// UseConfinedContainerGrain (MAD_CONTAINER_CONFINED=1), so its read-only /
// no-egress assertions hold WHEN CHOSEN — never relying on a confined-by-default that
// no longer exists.
//
// Everything here speaks the PUBLIC surface only (the substrate.provision RPC over
// the wire client) plus the SAME out-of-band tools the existing escape probe uses
// (raw `git`, and here the raw `container` CLI) acting as the uncooperative agent —
// it imports no forbidden internal package (readonly_imports_test still passes).
//
// CONTAINER HYGIENE (hard rule): every container a check provisions is torn down
// (substrate.teardown on the holding connection AND a belt-and-suspenders direct
// `container rm -f` on the captured id) so a failing assertion never leaks a
// container.

// containerBin is the runtime CLI the confinement tier conducts directly as the
// uncooperative agent (the same binary the substrate's grain conducts). Kept as a
// var so a future test could repoint it; production is the verified "container".
var containerBin = "container"

// ContainerRuntimeAvailable reports whether the Apple `container` runtime is usable:
// the CLI is on PATH AND a cheap probe (`container list`) reaches a running
// apiserver. A confinement check that finds it FALSE returns a SKIPPED Result with
// the reason — the gate stays runnable on a runtime-less CI host, never a silent
// pass and never a hard failure (matches substrate's requireContainerRuntime).
func ContainerRuntimeAvailable() (bool, string) {
	if _, err := exec.LookPath(containerBin); err != nil {
		return false, "`container` CLI not on PATH (runtime-less host / CI)"
	}
	if out, err := runContainerCLI("list", "-a", "-q"); err != nil {
		return false, fmt.Sprintf("`container` runtime not reachable (apiserver down): %v: %s", err, out)
	}
	return true, ""
}

// UseContainerGrain rebuilds the scratch daemon under MAD_GRAIN=container so
// every subsequent substrate.provision yields a CONFINED container boundary. It is
// the per-Scratch grain dial a confinement check flips at the top of Run/Control
// (the default Scratch stays the worktree grain — the existing gate is unchanged).
// It reuses writeManifestAndRestart's daemon-restart primitive but keeps the
// manifest empty (the container grain needs no manifest).
func (s *Scratch) UseContainerGrain() error {
	s.grain = "container"
	// Restart the daemon so its substrate re-reads the grain dial from the env.
	if s.daemon != nil && s.daemon.Process != nil {
		_ = s.daemon.Process.Kill()
		_, _ = s.daemon.Process.Wait()
		s.daemon = nil
	}
	return s.startDaemon()
}

// UseConfinedContainerGrain is UseContainerGrain plus the master CONFINEMENT opt-in:
// it pins MAD_CONTAINER_CONFINED=1 on the scratch daemon so every provisioned
// boundary gets the FULL confinement bundle — --cap-drop ALL, a --read-only rootfs, AND
// (because confined implies network none with no explicit override) NO egress. The
// container grain's COOPERATIVE default (no env var) gives default caps + a writable
// rootfs + the runtime's default network (egress) — consistent with the worktree grain's
// host posture — so the confinement tier must EXPLICITLY request confinement here to prove
// its read-only / no-egress assertions hold WHEN CHOSEN, never relying on a
// confined-by-default that no longer exists. (An explicit s.containerNetwork still wins
// over the implied none, so a check can pin a specific network independently.)
func (s *Scratch) UseConfinedContainerGrain() error {
	s.containerConfined = true
	return s.UseContainerGrain()
}

// ContainerBoundary is a provisioned container-grain boundary plus the means to
// tear it down. The holding connection is kept OPEN so Teardown runs on the SAME
// session that provisioned it (substrate.provision/teardown are connection-bound,
// Inv 4) — a fresh connection could not tear down another session's boundary.
type ContainerBoundary struct {
	Boundary
	conn *rpcclient.Client
	s    *Scratch
}

// ProvisionContainer provisions ONE container-grain boundary on a HELD connection
// (so it can be torn down on the same session) and returns it with the captured
// container id. The daemon MUST already be on the container grain (UseContainerGrain).
// The caller MUST defer Teardown — which removes the container and worktree and
// closes the connection (and, belt-and-suspenders, rm -f's the captured id so a
// teardown-RPC failure still never leaks a container).
func (s *Scratch) ProvisionContainer(resources ...provisionResource) (*ContainerBoundary, error) {
	c, err := s.Dial()
	if err != nil {
		return nil, err
	}
	params := map[string]any{}
	if len(resources) > 0 {
		params["resources"] = resources
	}
	var b Boundary
	if err := c.Call("substrate.provision", params, &b); err != nil {
		_ = c.Close()
		return nil, err
	}
	if b.Grain != "container" {
		// The daemon was not on the container grain — tear the boundary down and surface
		// the misconfiguration rather than silently asserting the wrong grain.
		_ = c.Call("substrate.teardown", map[string]any{}, &struct{}{})
		_ = c.Close()
		return nil, fmt.Errorf("expected a container-grain boundary, got grain %q (is MAD_GRAIN=container?)", b.Grain)
	}
	return &ContainerBoundary{Boundary: b, conn: c, s: s}, nil
}

// Teardown removes the container boundary on its holding connection (substrate.
// teardown), then closes the connection, then — belt-and-suspenders — rm -f's the
// captured container id directly so a teardown-RPC failure still leaks NOTHING.
// Idempotent and safe to defer.
func (cb *ContainerBoundary) Teardown() {
	if cb == nil {
		return
	}
	if cb.conn != nil {
		_ = cb.conn.Call("substrate.teardown", map[string]any{}, &struct{}{})
		_ = cb.conn.Close()
		cb.conn = nil
	}
	if cb.ContainerID != "" {
		_, _ = runContainerCLI("rm", "-f", cb.ContainerID)
	}
}

// ExecInContainer runs a command INSIDE the boundary's confined container as the
// uncooperative agent (raw `container exec`, no adapter, no cooperative protocol —
// exactly the launcher's container-exec path, driven directly). It returns the
// combined output and an error iff the in-container command exited non-zero. This is
// the surface the confinement checks drive: an agent that ignores the boundary and
// tries to read/write the host FS, reach the trunk, or open the network, executing
// in the SAME structural confinement a real agent would.
func (cb *ContainerBoundary) ExecInContainer(args ...string) (string, error) {
	if cb.ContainerID == "" {
		return "", fmt.Errorf("no container id on the boundary")
	}
	full := append([]string{"exec", cb.ContainerID}, args...)
	return runContainerCLI(full...)
}

// runContainerCLI conducts the `container` CLI and returns its trimmed combined
// output. It is the conformance package's own conductor (the package may not import
// internal/substrate), mirroring substrate's runContainer.
func runContainerCLI(args ...string) (string, error) {
	cmd := exec.Command(containerBin, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ----------------------------------------------------------------------------
// CONTROL-ONLY: a deliberately NON-confined container the confinement controls
// inject as the "breach" — it mounts a host path the confined grain never would, so
// the SAME in-container reachability predicate can be shown to FIRE (proving the
// negative is non-vacuous). These run a RAW `container run` (NOT through the grain),
// so they bypass the substrate's confinement on purpose; each is rm -f'd on rm().
// ----------------------------------------------------------------------------

// ctlContainerSeq makes control container names unique per process run.
var ctlContainerSeq atomic.Int64

// leakyContainer is a raw, NON-confined container started by a control. It wraps a
// minimal ContainerBoundary so the SAME ExecInContainer-based predicates apply.
type leakyContainer struct {
	id string
}

// boundary adapts the leaky container to a *ContainerBoundary (id only) so the
// in-container probe predicates (hostPathReadableInside / pathWritableInside) run
// against it unchanged — the control asserts they FIRE on a reachable path.
func (l *leakyContainer) boundary() *ContainerBoundary {
	return &ContainerBoundary{Boundary: Boundary{ContainerID: l.id}}
}

// rm force-removes the control container (idempotent), so a control never leaks one.
func (l *leakyContainer) rm() {
	if l != nil && l.id != "" {
		_, _ = runContainerCLI("rm", "-f", l.id)
	}
}

// runContainerWithHostMount starts a raw, NON-confined container that bind-mounts
// hostPath at its OWN identical ABSOLUTE path inside the container — so the SAME host
// path the confinement Run probes (e.g. the sentinel dir, the trunk repo, the socket
// dir) resolves and is reachable inside. This is the breach a confinement control
// injects: it then asserts the SAME in-container reachability predicate FIRES on this
// reachable path (proving the Run's "unreachable under confinement" is non-vacuous).
func (s *Scratch) runContainerWithHostMount(hostPath string) (*leakyContainer, error) {
	name := "nm-t3ctl-" + strconv.FormatInt(ctlContainerSeq.Add(1), 10) + "-" + filepathBase(s.Root)
	_, _ = runContainerCLI("rm", "-f", name) // prune a stale same-named container
	out, err := runContainerCLI(
		"run", "-d", "--name", name,
		// Mount at the IDENTICAL absolute host path so the Run's exact-path predicate
		// resolves inside (verified: the runtime accepts an absolute host-path target).
		"--mount", "type=bind,source="+hostPath+",target="+hostPath+",readonly",
		s.containerImage(),
		"sleep", "infinity",
	)
	if err != nil {
		_, _ = runContainerCLI("rm", "-f", name)
		return nil, fmt.Errorf("%w: %s", err, out)
	}
	return &leakyContainer{id: name}, nil
}

// runContainerDefaultNetwork starts a raw, NON-confined container on the DEFAULT
// network (no --network none) — the deliberately non-confined CONTROL for the
// network-confinement check: it gets an eth0 + a routable IP + working egress, so
// the SAME egress probe the Run asserts FAILS under confinement can be shown to
// SUCCEED here (proving the egress-blocked assertion is real, not a dead probe / a
// runtime that has no network at all). It still drops caps + read-only rootfs so the
// ONLY differing variable is the network namespace. rm -f'd on rm().
func (s *Scratch) runContainerDefaultNetwork() (*leakyContainer, error) {
	name := "nm-t3netctl-" + strconv.FormatInt(ctlContainerSeq.Add(1), 10) + "-" + filepathBase(s.Root)
	_, _ = runContainerCLI("rm", "-f", name)
	out, err := runContainerCLI(
		"run", "-d", "--name", name,
		// NO --network none → the default NAT network (eth0 + routable IP + egress).
		"--cap-drop", "ALL", "--read-only",
		s.containerImage(),
		"sleep", "infinity",
	)
	if err != nil {
		_, _ = runContainerCLI("rm", "-f", name)
		return nil, fmt.Errorf("%w: %s", err, out)
	}
	return &leakyContainer{id: name}, nil
}

// containerImage is the image the scratch's grain uses, mirroring the substrate's
// resolution (MAD_CONTAINER_IMAGE, default alpine:latest) so a control's raw
// run uses the SAME image the grain provisioned — the predicate is apples-to-apples.
func (s *Scratch) containerImage() string {
	if img := strings.TrimSpace(os.Getenv("MAD_CONTAINER_IMAGE")); img != "" {
		return img
	}
	return "alpine:latest"
}

// osWriteFile writes content to a host path with 0600 perms (the sentinel secret),
// creating the parent dir if needed — a tiny indirection so check_confinement.go
// needs no direct os import for this.
func osWriteFile(path, content string) error {
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		if err := os.MkdirAll(path[:i], 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// filepathBase returns the last path element, used to name control containers
// distinctively per scratch (avoids importing path/filepath just for this here).
func filepathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
