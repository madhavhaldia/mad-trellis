package launcher

// Tests for the grain-aware exec seam (T3 stage 2). The pure-unit tests pin the
// host path as BYTE-IDENTICAL and the container argv shape WITHOUT a runtime; the
// LIVE test runs a fake agent INSIDE a real confined container and SKIPs with a
// clear reason when the `container` runtime is unavailable (CI / runtime-less
// host) — never a silent pass. Every container created is rm -f'd on a deferred
// cleanup, so a failed test still leaks nothing.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// --- PURE UNIT (no runtime): exec-command construction --------------------------

// The worktree/host grain must build EXACTLY the command the pre-grain RunPTY did:
// exec.Command(agent, args) in cwd, env = MergeEnv(os.Environ(), spec). Any drift
// here is a behavior change to the default grain (forbidden).
func TestBuildExecCommandHostPathIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"MAD_SESSION": "s-1", "PORT": "5000"}
	for _, grain := range []string{"", "worktree"} {
		c, err := buildExecCommand(ExecTarget{Grain: grain, Cwd: dir}, env, "myagent", []string{"--flag", "v"})
		if err != nil {
			t.Fatalf("grain %q: unexpected error: %v", grain, err)
		}
		if c.Dir != dir {
			t.Errorf("grain %q: Dir = %q, want %q", grain, c.Dir, dir)
		}
		// argv[0] is the resolved path of "myagent" (or "myagent" itself if not on
		// PATH); the trailing args must be forwarded verbatim with no injection.
		if got := c.Args[len(c.Args)-2:]; got[0] != "--flag" || got[1] != "v" {
			t.Errorf("grain %q: args not forwarded verbatim: %v", grain, c.Args)
		}
		// env must equal MergeEnv(os.Environ(), env) exactly (per-spec wins, base inherits).
		want := MergeEnv(os.Environ(), env)
		if strings.Join(c.Env, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("grain %q: env != MergeEnv(os.Environ(), spec)", grain)
		}
		// The host path must invoke the agent directly, never the container runtime.
		if c.Args[0] != "myagent" {
			t.Errorf("grain %q: host path argv[0] = %q, want the agent itself", grain, c.Args[0])
		}
	}
}

// The container grain must build: container exec -i -t -w /work -e K=V ... <id> <agent> <args...>
// with the governed env carried as -e flags (sorted, deterministic) and the agent
// + its args appended verbatim after the id.
func TestBuildExecCommandContainerArgv(t *testing.T) {
	// Pin the runtime binary name so the assertion does not depend on PATH.
	old := containerBin
	containerBin = "container"
	defer func() { containerBin = old }()

	env := map[string]string{"MAD_SESSION": "s-9", "MAD_SESSION_TOKEN": "tok", "PORT": "7000"}
	c, err := buildExecCommand(ExecTarget{Grain: "container", Cwd: "/work", ContainerID: "ctr-abc"}, env, "claude", []string{"--resume"})
	if err != nil {
		t.Fatalf("container buildExecCommand: %v", err)
	}
	got := strings.Join(c.Args, " ")
	for _, want := range []string{
		"exec -i -t -w /work",
		"-e MAD_SESSION=s-9",
		"-e MAD_SESSION_TOKEN=tok",
		"-e PORT=7000",
		"ctr-abc claude --resume",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("container argv %q missing %q", got, want)
		}
	}
	// -e flags must be SORTED (deterministic argv): MAD_SESSION before _TOKEN before PORT.
	iS := strings.Index(got, "MAD_SESSION=")
	iT := strings.Index(got, "MAD_SESSION_TOKEN=")
	iP := strings.Index(got, "PORT=")
	if !(iS < iT && iT < iP) {
		t.Errorf("env -e flags not sorted: SESSION@%d TOKEN@%d PORT@%d (%q)", iS, iT, iP, got)
	}
	// The id+agent+args must come AFTER every -e flag (agent never mistaken for a flag value).
	if strings.Index(got, "ctr-abc") < iP {
		t.Errorf("container id appears before the env flags: %q", got)
	}
}

// FAIL-CLOSED: a "container" grain with no container id is an inconsistent wire and
// must NOT silently run on the host — buildExecCommand errors and runPTYIO maps it
// NON-ROOT EXEC USER: MAD_CONTAINER_USER (when safe) adds `--user <value>` to
// the container-exec argv — the enabler for `claude --dangerously-skip-permissions`,
// which claude refuses as root. UNSET ⇒ NO --user (byte-identical to the root default);
// an UNSAFE value is IGNORED (no --user, no arg-injection).
func TestBuildContainerExecUserFlag(t *testing.T) {
	old := containerBin
	containerBin = "container"
	defer func() { containerBin = old }()

	// SET + safe (a bare name): --user <value> present.
	t.Setenv("MAD_CONTAINER_USER", "node")
	if got := strings.Join(buildContainerExec("ctr-1", map[string]string{"K": "V"}, "claude", []string{"--dangerously-skip-permissions"}).Args, " "); !strings.Contains(got, "--user node") {
		t.Errorf("set container user: argv missing `--user node`: %q", got)
	}
	// SET + safe (a uid:gid pair): also honored.
	t.Setenv("MAD_CONTAINER_USER", "1000:1000")
	if got := strings.Join(buildContainerExec("ctr-1", nil, "claude", nil).Args, " "); !strings.Contains(got, "--user 1000:1000") {
		t.Errorf("uid:gid user: argv missing `--user 1000:1000`: %q", got)
	}

	// UNSET ⇒ NO --user (byte-identical to today's root default).
	os.Unsetenv("MAD_CONTAINER_USER")
	if got := strings.Join(buildContainerExec("ctr-1", nil, "claude", nil).Args, " "); strings.Contains(got, "--user") {
		t.Errorf("unset container user: argv must NOT contain --user: %q", got)
	}

	// CONTROL — unsafe values are IGNORED (no --user, never arg-injected). Each row
	// would slip a flag/metachar into the runtime argv under a naive passthrough.
	for _, bad := range []string{"-rm", "ro ot", "a;b", "x$y", "--privileged"} {
		t.Setenv("MAD_CONTAINER_USER", bad)
		if got := strings.Join(buildContainerExec("ctr-1", nil, "claude", nil).Args, " "); strings.Contains(got, "--user") {
			t.Errorf("unsafe user %q must be ignored, argv=%q", bad, got)
		}
	}
}

// to BlockedExitCode (the agent is never reached).
func TestContainerGrainNoIDFailsClosed(t *testing.T) {
	_, err := buildExecCommand(ExecTarget{Grain: "container", ContainerID: ""}, nil, "claude", nil)
	if !errors.Is(err, errMissingContainerID) {
		t.Fatalf("want errMissingContainerID, got %v", err)
	}
	// Through the PTY core: fail-closed BlockedExitCode, agent never reached.
	var out bytes.Buffer
	code, err := runPTYIO(strings.NewReader(""), &out, ExecTarget{Grain: "container"}, nil, "claude", nil)
	if code != BlockedExitCode {
		t.Errorf("exit code = %d, want BlockedExitCode %d (fail-closed)", code, BlockedExitCode)
	}
	if err == nil {
		t.Error("want a non-nil error explaining the fail-closed BLOCK")
	}
}

// FAIL-CLOSED: an UNKNOWN/future/typo grain must NOT silently run on the host (the
// old fall-through bug). buildExecCommand is an explicit allowlist — an unrecognized
// grain returns errUnknownGrain, which runPTYIO maps to BlockedExitCode.
func TestUnknownGrainFailsClosed(t *testing.T) {
	for _, g := range []string{"vm", "Container", "worktre", "bogus", "host"} {
		_, err := buildExecCommand(ExecTarget{Grain: g, Cwd: t.TempDir()}, nil, "claude", nil)
		if !errors.Is(err, errUnknownGrain) {
			t.Fatalf("grain %q: want errUnknownGrain (no silent host run), got %v", g, err)
		}
		// Through the PTY core: fail-closed BlockedExitCode, agent never reached.
		var out bytes.Buffer
		code, perr := runPTYIO(strings.NewReader(""), &out, ExecTarget{Grain: g}, nil, "claude", nil)
		if code != BlockedExitCode {
			t.Errorf("grain %q: exit code = %d, want BlockedExitCode %d (fail-closed)", g, code, BlockedExitCode)
		}
		if perr == nil {
			t.Errorf("grain %q: want a non-nil error explaining the fail-closed BLOCK", g)
		}
	}
}

// --- LIVE (real runtime): run a fake agent INSIDE a confined container ----------

// requireContainerRuntime skips unless the `container` CLI is on PATH AND a cheap
// probe reaches a running apiserver — never a silent pass, never a hard failure.
func requireContainerRuntime(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("container-exec test: `container` CLI not on PATH (runtime-less host)")
	}
	if out, err := exec.Command("container", "list", "-a", "-q").CombinedOutput(); err != nil {
		t.Skipf("container-exec test: runtime not reachable (apiserver down): %s", strings.TrimSpace(string(out)))
	}
}

// containerImage mirrors the substrate default (override per host).
func containerImage() string {
	if v := strings.TrimSpace(os.Getenv("MAD_CONTAINER_IMAGE")); v != "" {
		return v
	}
	return "alpine:latest"
}

// TestRunPTYContainerGrainRunsAgentInsideContainer is the stage-2 end-to-end
// assertion: RunPTY against a container boundary execs the fake agent INSIDE the
// container at /work (not on the host), the exit code propagates, and the
// container is torn down after. It stands up a confined container exactly like the
// substrate grain (cap-drop ALL, read-only rootfs, --network none, only the
// worktree mounted at /work), runs the agent via the real container-exec path,
// then tears it down and asserts it is gone.
func TestRunPTYContainerGrainRunsAgentInsideContainer(t *testing.T) {
	requireContainerRuntime(t)

	// A host worktree dir with a seed file, bind-mounted at /work — the ONLY host FS
	// the container can see (structural confinement).
	hostWork := t.TempDir()
	if err := os.WriteFile(hostWork+"/seed.txt", []byte("seeded-by-host\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	name := "nm-t3test-" + randSuffix(t)
	// Best-effort prune of any stale same-named container, then a deferred rm -f so
	// a failed assertion never leaks a container.
	_ = exec.Command("container", "rm", "-f", name).Run()
	defer func() { _ = exec.Command("container", "rm", "-f", name).Run() }()

	runArgs := []string{
		"run", "-d", "--name", name,
		"--mount", "type=bind,source=" + hostWork + ",target=/work",
		"-w", "/work",
		"--cap-drop", "ALL",
		"--read-only",
		"--network", "none",
		containerImage(),
		"sleep", "infinity",
	}
	if out, err := exec.Command("container", runArgs...).CombinedOutput(); err != nil {
		t.Fatalf("container run: %v: %s", err, strings.TrimSpace(string(out)))
	}

	// Drive the REAL grain-aware exec path. The fake agent proves it ran INSIDE the
	// container: it echoes a marker, prints its cwd (must be /work), reads the
	// host-seeded file through the mount, and exposes the governed env var.
	var out bytes.Buffer
	env := map[string]string{"MAD_SESSION": "s-live", "MAD_SESSION_TOKEN": "tok-live"}
	target := ExecTarget{Grain: "container", Cwd: "/work", ContainerID: name}
	code, err := runPTYIO(strings.NewReader(""), &out,
		target, env,
		"sh", []string{"-c", `printf 'MARK=governed-in-container CWD=%s SEED=%s SESS=%s' "$(pwd)" "$(cat /work/seed.txt)" "$MAD_SESSION"`},
	)
	if err != nil {
		t.Fatalf("runPTYIO (container exec): %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (clean in-container child exit propagated)", code)
	}
	got := out.String()
	for _, want := range []string{
		"MARK=governed-in-container",
		"CWD=/work",           // ran INSIDE the container at /work, not the host cwd
		"SEED=seeded-by-host", // saw the host worktree through the bind mount
		"SESS=s-live",         // governed env-spec injected via -e
	} {
		if !strings.Contains(got, want) {
			t.Errorf("in-container output %q missing %q", got, want)
		}
	}
	// It must NOT have run on the host: the host temp dir is never /work.
	if strings.Contains(got, "CWD="+hostWork) {
		t.Errorf("agent ran on the HOST (cwd=%s), not inside the container", hostWork)
	}

	// Exit-code propagation through the container-exec path: a non-zero in-container
	// exit must surface exactly.
	var out2 bytes.Buffer
	code2, err := runPTYIO(strings.NewReader(""), &out2, target, env, "sh", []string{"-c", "exit 7"})
	if err != nil {
		t.Fatalf("runPTYIO (container exec, exit 7): %v", err)
	}
	if code2 != 7 {
		t.Errorf("in-container exit code = %d, want 7 (propagated through container exec)", code2)
	}

	// Teardown: remove the container and assert it is gone (no leak).
	if out, err := exec.Command("container", "rm", "-f", name).CombinedOutput(); err != nil {
		t.Fatalf("container rm: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// Give the runtime a moment, then confirm absence.
	time.Sleep(200 * time.Millisecond)
	listOut, _ := exec.Command("container", "list", "-a", "-q").CombinedOutput()
	for _, line := range strings.Split(string(listOut), "\n") {
		if strings.TrimSpace(line) == name {
			t.Errorf("container %q still present after teardown (leak)", name)
		}
	}
}

// randSuffix returns a short random hex suffix for distinctive test container names.
func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}
