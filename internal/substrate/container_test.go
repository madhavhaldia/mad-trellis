package substrate

// Hand-authored GATED tests for the T3 CONTAINER GRAIN (chafe C5/C13): the grain
// dial realized as STRUCTURAL confinement of an uncooperative agent. These run
// LIVE against the Apple `container` runtime and SKIP-with-a-clear-reason when it
// is unavailable (CI / a runtime-less host) — never a silent pass, never a hard
// failure. Every container created is rm -f'd on a deferred cleanup, so a failed
// test still leaks nothing.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testContainerImage mirrors newContainerGrain's image resolution so a control's
// raw run uses the SAME image the grain provisions.
func testContainerImage() string {
	if v := strings.TrimSpace(os.Getenv("MAD_CONTAINER_IMAGE")); v != "" {
		return v
	}
	return defaultContainerImage
}

// requireContainerRuntime skips the test unless the `container` CLI is on PATH
// AND a cheap probe (`container list`) actually reaches a running apiserver.
func requireContainerRuntime(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("container grain test: `container` CLI not on PATH (runtime-less host)")
	}
	if _, err := runContainer("list", "-a", "-q"); err != nil {
		t.Skip("container grain test: `container` runtime not reachable (apiserver down)")
	}
}

// containerExists reports whether the runtime currently lists a container with
// the given id (running OR stopped: -a).
func containerExists(t *testing.T, id string) bool {
	t.Helper()
	out, err := runContainer("list", "-a", "-q")
	if err != nil {
		t.Fatalf("container list: %v: %s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.TrimSpace(line)
		// `container` may print a short id; match on prefix in both directions.
		if f != "" && (f == id || strings.HasPrefix(id, f) || strings.HasPrefix(f, id)) {
			return true
		}
	}
	return false
}

// newContainerSub builds a Substrate over a fresh git repo, selecting the
// container grain via the dial, with isolated worktree/state roots (nothing
// touches $HOME).
func newContainerSub(t *testing.T) *Substrate {
	t.Helper()
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	t.Setenv("MAD_STATE_DIR", t.TempDir())
	repo := initRepo(t)
	s, err := New(repo, nil, Options{GrainName: "container"})
	if err != nil {
		t.Fatalf("New(container grain): %v", err)
	}
	return s
}

// TestContainerGrainProvisionConfinesAndMounts is the end-to-end T3 assertion:
// provision a container boundary, prove the container is RUNNING and the agent's
// worktree is mounted at /work (the seed file is visible inside), prove the
// CONFINEMENT (a host sentinel written OUTSIDE the mount is NOT visible inside),
// then teardown removes both the container and the host worktree.
func TestContainerGrainProvisionConfinesAndMounts(t *testing.T) {
	requireContainerRuntime(t)
	s := newContainerSub(t)

	spec, err := s.Provision("s-1-aaaa", Request{Ports: 2})
	if err != nil {
		t.Fatalf("provision (container grain): %v", err)
	}
	cid := spec.containerID
	if cid == "" {
		t.Fatal("container grain must capture a container id")
	}
	// Deferred cleanup: rm -f the container even if an assertion fails below, so a
	// failing test never leaks a container.
	t.Cleanup(func() { rmContainer(cid) })

	// The boundary contract: cwd is the IN-CONTAINER mount; the host worktree is a
	// real dir OUTSIDE the governed repo; the wire projects the id+host path.
	if spec.cwd != containerWorkMount {
		t.Fatalf("agent cwd must be the in-container mount %q, got %q", containerWorkMount, spec.cwd)
	}
	if spec.hostWorktree == "" {
		t.Fatal("container boundary must carry the host worktree path")
	}
	if _, err := os.Stat(spec.hostWorktree); err != nil {
		t.Fatalf("host worktree must exist on the host: %v", err)
	}
	if !disjoint(spec.hostWorktree, s.repoAbs) {
		t.Fatalf("host worktree %q must be outside the governed repo %q", spec.hostWorktree, s.repoAbs)
	}
	w := spec.Wire()
	if w.ContainerID != cid || w.HostWorktree != spec.hostWorktree || w.Grain != "container" {
		t.Fatalf("wire must project the container fields: %+v", w)
	}

	// (1) The container is RUNNING.
	if !containerExists(t, cid) {
		t.Fatalf("provisioned container %q must be listed as running", cid)
	}

	// (2) The worktree is mounted at /work — the repo's seed file is visible inside.
	out, err := runContainer("exec", cid, "ls", containerWorkMount)
	if err != nil {
		t.Fatalf("exec ls /work: %v: %s", err, out)
	}
	if !strings.Contains(out, "README.md") {
		t.Fatalf("the worktree seed file must be visible at /work inside the container, got:\n%s", out)
	}
	// And readable through the mount.
	out, err = runContainer("exec", cid, "cat", containerWorkMount+"/README.md")
	if err != nil {
		t.Fatalf("exec cat /work/README.md: %v: %s", err, out)
	}
	if !strings.Contains(out, "seed") {
		t.Fatalf("the seed file's contents must be readable at /work, got:\n%s", out)
	}

	// (3) CONFINEMENT smoke: a host sentinel written OUTSIDE the mount (in the
	// substrate's state root, a real host dir) must NOT be reachable from inside —
	// the container sees ONLY its own worktree, not the broader host FS.
	sentinelDir := t.TempDir()
	sentinel := filepath.Join(sentinelDir, "HOST_SENTINEL")
	if err := os.WriteFile(sentinel, []byte("host-only"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The exact host path is not reachable inside (no such mount).
	if out, err := runContainer("exec", cid, "cat", sentinel); err == nil {
		t.Fatalf("CONFINEMENT VIOLATED: a host path outside the mount was readable inside the container:\n%s", out)
	}
	// Positive control: the container CAN see its own root (it is a real FS),
	// proving the negative above is non-vacuous (exec works, the path is just absent).
	if out, err := runContainer("exec", cid, "ls", "/"); err != nil {
		t.Fatalf("control vacuous: exec must work for a benign command: %v: %s", err, out)
	}

	// (4) Teardown removes the container AND the host worktree.
	if err := s.Teardown("s-1-aaaa"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if containerExists(t, cid) {
		t.Fatalf("teardown must remove the container %q from the runtime", cid)
	}
	if _, err := os.Stat(spec.hostWorktree); !os.IsNotExist(err) {
		t.Fatal("teardown must remove the host worktree")
	}
	if s.Lookup("s-1-aaaa") != nil {
		t.Fatal("session must not be live after teardown")
	}
}

// TestContainerGrainRestartReclaimsOrphan REPRODUCES the reviewer's daemon-restart
// leak (FIX 1): substrate A (container grain) provisions a RUNNING container +
// host worktree, then a FRESH substrate B over the SAME repo/grain (the restarted
// daemon, with an EMPTY live map) calls Teardown(session). Before the fix, the
// live-map MISS made Teardown a no-op and the container/worktree leaked FOREVER.
// After the fix, B's Teardown reclaims the orphan by its DETERMINISTIC name/path:
// the container nm-<slug> is GONE and the host worktree dir is removed.
func TestContainerGrainRestartReclaimsOrphan(t *testing.T) {
	requireContainerRuntime(t)
	// Shared roots so substrate B derives the SAME deterministic paths/names as A.
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	t.Setenv("MAD_STATE_DIR", t.TempDir())
	repo := initRepo(t)

	// Substrate A: the original daemon. Provision a live container boundary.
	a, err := New(repo, nil, Options{GrainName: "container"})
	if err != nil {
		t.Fatalf("New(A): %v", err)
	}
	const session = "s-restart-aaaa"
	spec, err := a.Provision(session, Request{Ports: 2})
	if err != nil {
		t.Fatalf("provision (A): %v", err)
	}
	cid := spec.containerID
	name := "nm-" + safeSlug(session)
	host := spec.hostWorktree
	// Belt-and-suspenders: rm -f even if the test fails, so nothing leaks.
	t.Cleanup(func() { rmContainer(cid); rmContainer(name) })

	if !containerExists(t, cid) {
		t.Fatalf("precondition: the provisioned container %q must be running", cid)
	}
	if _, err := os.Stat(host); err != nil {
		t.Fatalf("precondition: the host worktree %q must exist: %v", host, err)
	}

	// SIMULATE THE DAEMON RESTART: a fresh Substrate over the SAME repo+grain has an
	// EMPTY live map — exactly what app.Build creates on restart. The dead holder's
	// session is reconstructed by liveness from the durable lease ledger and routed
	// to substrate.Teardown, which here hits the live-map MISS path.
	b, err := New(repo, nil, Options{GrainName: "container"})
	if err != nil {
		t.Fatalf("New(B, restarted daemon): %v", err)
	}
	if b.Lookup(session) != nil {
		t.Fatal("the restarted substrate must have NO live boundary for the session")
	}
	if err := b.Teardown(session); err != nil {
		t.Fatalf("restarted Teardown must reclaim the orphan, not error: %v", err)
	}

	// THE FIX: the orphaned container is GONE and the host worktree is removed.
	if containerExists(t, name) || containerExists(t, cid) {
		t.Fatalf("LEAK: the orphaned container %q (id %q) survived the restarted daemon's Teardown", name, cid)
	}
	if _, err := os.Stat(host); !os.IsNotExist(err) {
		t.Fatalf("LEAK: the orphaned host worktree %q survived the restarted daemon's Teardown (stat err=%v)", host, err)
	}
}

// TestWorktreeGrainRestartReclaimsOrphan is the worktree grain's analogous
// stale-dir orphan reclaim: a fresh substrate's Teardown (empty live map) removes
// the reconstructed worktree dir for the session. No runtime needed.
func TestWorktreeGrainRestartReclaimsOrphan(t *testing.T) {
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	t.Setenv("MAD_STATE_DIR", t.TempDir())
	repo := initRepo(t)

	a, err := New(repo, nil, Options{GrainName: "worktree"})
	if err != nil {
		t.Fatalf("New(A): %v", err)
	}
	const session = "s-wtrestart-bbbb"
	spec, err := a.Provision(session, Request{})
	if err != nil {
		t.Fatalf("provision (A): %v", err)
	}
	host := spec.hostWorktree
	if _, err := os.Stat(host); err != nil {
		t.Fatalf("precondition: host worktree %q must exist: %v", host, err)
	}

	// Restart: fresh substrate, empty live map.
	b, err := New(repo, nil, Options{GrainName: "worktree"})
	if err != nil {
		t.Fatalf("New(B): %v", err)
	}
	if err := b.Teardown(session); err != nil {
		t.Fatalf("restarted Teardown must reclaim the orphan worktree: %v", err)
	}
	if _, err := os.Stat(host); !os.IsNotExist(err) {
		t.Fatalf("LEAK: orphaned worktree %q survived the restarted daemon's Teardown (err=%v)", host, err)
	}
}

// TestContainerGrainWritableScratchAndTmp proves FIX 3: a real agent honoring the
// injected TMPDIR/XDG/MAD_* env can actually WRITE — the per-agent state dir
// is bind-mounted writable at its host path and /tmp is a writable tmpfs — WITHOUT
// weakening confinement (a host sentinel OUTSIDE the mounts stays unreachable).
func TestContainerGrainWritableScratchAndTmp(t *testing.T) {
	requireContainerRuntime(t)
	s := newContainerSub(t)

	spec, err := s.Provision("s-scratch-cccc", Request{Ports: 1})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	cid := spec.containerID
	t.Cleanup(func() { rmContainer(cid) })
	defer func() { _ = s.Teardown("s-scratch-cccc") }()

	// The injected env the agent would honor.
	tmpdir := spec.env["TMPDIR"]
	cache := spec.env["XDG_CACHE_HOME"]
	state := spec.env["XDG_STATE_HOME"]
	if tmpdir == "" || cache == "" || state == "" {
		t.Fatalf("substrate must inject TMPDIR/XDG_CACHE_HOME/XDG_STATE_HOME: %+v", spec.env)
	}

	// (1) A write to the injected TMPDIR (host path) SUCCEEDS inside the container.
	for _, p := range []string{tmpdir, cache, state} {
		out, err := runContainer("exec", cid, "sh", "-c", "echo nm > "+p+"/probe && cat "+p+"/probe")
		if err != nil || !strings.Contains(out, "nm") {
			t.Fatalf("writable injected path %q must succeed inside the container: %v: %s", p, err, out)
		}
	}
	// (2) /tmp is writable (tmpfs).
	if out, err := runContainer("exec", cid, "sh", "-c", "echo t > /tmp/x && cat /tmp/x"); err != nil || !strings.Contains(out, "t") {
		t.Fatalf("/tmp must be writable inside the container: %v: %s", err, out)
	}
	// (3) CONFINEMENT INTACT: a host sentinel OUTSIDE all mounts is unreachable. The
	// state ROOT (MAD_STATE_DIR) sits ABOVE the per-agent mount, so a sentinel
	// there is on the host but not in any mount.
	sentinel := filepath.Join(os.Getenv("MAD_STATE_DIR"), "HOST_SENTINEL_OUTSIDE")
	if err := os.WriteFile(sentinel, []byte("host-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runContainer("exec", cid, "test", "-e", sentinel); err == nil {
		t.Fatalf("CONFINEMENT VIOLATED: a host sentinel outside the mounts was reachable inside: %s", out)
	}
	// (4) The DEFAULT grain is now COOPERATIVE: the rootfs is WRITABLE (confinement is
	// the opt-in), so a write to an in-container rootfs path SUCCEEDS. This only touches
	// the container's OWN ephemeral rootfs — host isolation is MOUNT-STRUCTURAL (step 3
	// above: the host sentinel outside the mounts stays unreachable) and holds regardless
	// of read-only. The read-only rootfs is exercised separately by
	// TestContainerGrainConfinedReadonlyRootfs under MAD_CONTAINER_CONFINED=1.
	if out, err := runContainer("exec", cid, "sh", "-c", "echo e > /escape-attempt 2>/dev/null && echo WROTE"); err != nil || !strings.Contains(out, "WROTE") {
		t.Fatalf("cooperative DEFAULT rootfs must be WRITABLE (confinement is opt-in), write failed: %v: %s", err, out)
	}
}

// TestContainerGrainConfinedReadonlyRootfs proves the CONFINEMENT OPT-IN at the grain
// level: under MAD_CONTAINER_CONFINED=1 the provisioned container has a READ-ONLY
// rootfs, so a write to an in-container rootfs path FAILS — while the DEFAULT cooperative
// grain (no env) leaves it writable (asserted by TestContainerGrainWritableScratchAndTmp
// step 4, the non-vacuous contrast). The agent's own /work + /tmp stay writable even when
// confined, and the host sentinel outside the mounts is unreachable in BOTH modes.
func TestContainerGrainConfinedReadonlyRootfs(t *testing.T) {
	requireContainerRuntime(t)
	// Opt into the full CONFINEMENT bundle (read-only rootfs + cap-drop ALL + no egress).
	t.Setenv("MAD_CONTAINER_CONFINED", "1")
	s := newContainerSub(t)

	spec, err := s.Provision("s-confined-ffff", Request{Ports: 1})
	if err != nil {
		t.Fatalf("provision (confined): %v", err)
	}
	cid := spec.containerID
	t.Cleanup(func() { rmContainer(cid) })
	defer func() { _ = s.Teardown("s-confined-ffff") }()

	// (1) READ-ONLY rootfs: a write to an in-container rootfs path FAILS.
	if out, err := runContainer("exec", cid, "sh", "-c", "echo e > /escape-attempt 2>/dev/null && echo WROTE"); err == nil && strings.Contains(out, "WROTE") {
		t.Fatalf("CONFINED rootfs must be READ-ONLY (write to /escape-attempt must fail): %s", out)
	}
	// (2) /work and /tmp stay writable even when confined (a real agent must still run).
	if out, err := runContainer("exec", cid, "sh", "-c", "echo w > /work/agent.txt && cat /work/agent.txt"); err != nil || !strings.Contains(out, "w") {
		t.Fatalf("/work must be writable even when confined: %v: %s", err, out)
	}
	if out, err := runContainer("exec", cid, "sh", "-c", "echo t > /tmp/x && cat /tmp/x"); err != nil || !strings.Contains(out, "t") {
		t.Fatalf("/tmp must be writable even when confined: %v: %s", err, out)
	}
	// (3) Confined implies NO egress (network none, no explicit override): a connect to a
	// routable IP FAILS.
	if out, err := runContainer("exec", cid, "sh", "-c", "wget -T5 -O- http://1.1.1.1 >/dev/null 2>&1 && echo EGRESS-OPEN || echo EGRESS-BLOCKED"); err != nil || !strings.Contains(out, "EGRESS-BLOCKED") {
		t.Fatalf("confined grain must imply NO egress (network none): %v: %s", err, out)
	}
}

// TestContainerGrainNetworkNone proves the CONFINED OPT-IN at the grain level: with
// MAD_CONTAINER_NETWORK=none the provisioned container has NO egress
// (--network none) — a connect to a routable IP FAILS — while a CONTROL
// default-network container's same probe SUCCEEDS (non-vacuous). (The grain's
// cooperative DEFAULT — no env var — instead omits --network and HAS egress; this
// test exercises the confined mode that must still hold when explicitly chosen.)
func TestContainerGrainNetworkNone(t *testing.T) {
	requireContainerRuntime(t)
	// Opt into the CONFINED mode (cooperative egress is now the default).
	t.Setenv("MAD_CONTAINER_NETWORK", "none")
	s := newContainerSub(t)

	spec, err := s.Provision("s-net-dddd", Request{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	cid := spec.containerID
	t.Cleanup(func() { rmContainer(cid) })
	defer func() { _ = s.Teardown("s-net-dddd") }()

	// Confined: egress to a routable IP FAILS, and there is no eth0.
	if out, err := runContainer("exec", cid, "sh", "-c", "wget -T5 -O- http://1.1.1.1 >/dev/null 2>&1 && echo EGRESS-OPEN || echo EGRESS-BLOCKED"); err != nil || !strings.Contains(out, "EGRESS-BLOCKED") {
		t.Fatalf("confined container must have NO egress (--network none): %v: %s", err, out)
	}

	// CONTROL (non-vacuous): a default-network container reaches the SAME IP, proving
	// the block is the namespace removal, not a dead runtime network.
	ctl := "nm-fix-netctl-" + safeSlug(spec.session)
	rmContainer(ctl)
	if out, err := runContainer("run", "-d", "--name", ctl, "--cap-drop", "ALL", "--read-only", testContainerImage(), "sleep", "infinity"); err != nil {
		t.Fatalf("control default-network run: %v: %s", err, out)
	}
	t.Cleanup(func() { rmContainer(ctl) })
	if out, err := runContainer("exec", ctl, "sh", "-c", "wget -T5 -O- http://1.1.1.1 >/dev/null 2>&1 && echo EGRESS-OPEN || echo EGRESS-BLOCKED"); err != nil || !strings.Contains(out, "EGRESS-OPEN") {
		t.Fatalf("CONTROL VACUOUS: a default-network container had no egress either (runtime has no network?): %v: %s", err, out)
	}
}

// TestContainerGrainTeardownIdempotent: a teardown of a never-provisioned session
// is a no-op, and the grain's own Teardown ignores an already-gone container.
func TestContainerGrainTeardownIdempotent(t *testing.T) {
	requireContainerRuntime(t)
	s := newContainerSub(t)
	if err := s.Teardown("s-never"); err != nil {
		t.Fatalf("teardown of an unprovisioned session must be a no-op: %v", err)
	}
	// A grain Teardown with a bogus container id must not error (idempotent).
	g := newContainerGrain(s.repoAbs)
	if err := g.Teardown(Boundary{ContainerID: "nm-does-not-exist-zzzz"}); err != nil {
		t.Fatalf("grain teardown of a missing container must be idempotent, got: %v", err)
	}
}

// TestContainerGrainDialSelection: the dial selects the container grain by name
// and by env, and rejects an unknown grain — without needing the runtime.
func TestContainerGrainDialSelection(t *testing.T) {
	repo := t.TempDir()
	// By Options.GrainName.
	s, err := New(repo, nil, Options{GrainName: "container"})
	if err != nil {
		t.Fatalf("New(GrainName=container): %v", err)
	}
	if s.grain.Name() != "container" {
		t.Fatalf("dial must select the container grain, got %q", s.grain.Name())
	}
	// By env.
	t.Setenv("MAD_GRAIN", "container")
	s2, err := New(repo, nil, Options{})
	if err != nil {
		t.Fatalf("New(MAD_GRAIN=container): %v", err)
	}
	if s2.grain.Name() != "container" {
		t.Fatalf("env dial must select the container grain, got %q", s2.grain.Name())
	}
	// DEFAULT stays worktree.
	t.Setenv("MAD_GRAIN", "")
	s3, err := New(repo, nil, Options{})
	if err != nil {
		t.Fatalf("New(default): %v", err)
	}
	if s3.grain.Name() != "worktree" {
		t.Fatalf("default dial must stay the worktree grain, got %q", s3.grain.Name())
	}
	// Unknown grain → error.
	if _, err := New(repo, nil, Options{GrainName: "bogus"}); err == nil {
		t.Fatal("an unknown grain name must be rejected")
	}
	// Image is configurable via env.
	t.Setenv("MAD_CONTAINER_IMAGE", "busybox:latest")
	if g := newContainerGrain(repo); g.image != "busybox:latest" {
		t.Fatalf("MAD_CONTAINER_IMAGE must configure the image, got %q", g.image)
	}
}
