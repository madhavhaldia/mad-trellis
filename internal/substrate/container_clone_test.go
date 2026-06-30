package substrate

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/madhavhaldia/mad-substrate/internal/worktree"
)

// flagValue returns the argument following the first occurrence of flag in args
// (e.g. the value of "--network"), or "" if absent.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func argsContain(args []string, want ...string) bool {
	// want is a contiguous subsequence (e.g. "--cap-drop","ALL").
	for i := 0; i+len(want) <= len(args); i++ {
		ok := true
		for j, w := range want {
			if args[i+j] != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// TestContainerRunArgs proves the flag set WITHOUT a runtime: the COOPERATIVE default
// (confined=false) OMITS --cap-drop / --read-only / --network entirely (default caps +
// writable rootfs + the runtime's default network = egress), while the CONFINED opt-in
// (confined=true) adds --cap-drop ALL + --read-only; the network is an independent
// parameter ("" omits --network, "none" passes --network none, any other value passes
// it through). The two bind mounts, the unconditional writable /tmp, and the held
// command are always present regardless of mode.
func TestContainerRunArgs(t *testing.T) {
	// (1) COOPERATIVE default (confined=false, network="") OMITS --network, --cap-drop,
	// and --read-only → default caps + a WRITABLE rootfs + the runtime default network.
	coop := containerRunArgs("nm-x", "/tmp/cid", "/work", "/host/clone", "/host/state", "img:tag", "", false, nil, "linux/arm64")
	if argsContain(coop, "--network") {
		t.Fatalf("cooperative default must OMIT --network (runtime default network), got: %v", coop)
	}
	if argsContain(coop, "--cap-drop", "ALL") || argsContain(coop, "--read-only") {
		t.Fatalf("cooperative default must OMIT --cap-drop/--read-only (writable rootfs + default caps), got: %v", coop)
	}
	for _, must := range [][]string{
		{"--tmpfs", "/tmp"}, // unconditional, both modes
		{"-w", "/work"},
		{"--mount", "type=bind,source=/host/clone,target=/work"},
		{"--mount", "type=bind,source=/host/state,target=/host/state"},
		// The hold overrides any image ENTRYPOINT so `sleep infinity` always holds.
		{"--entrypoint", "sleep"},
	} {
		if !argsContain(coop, must...) {
			t.Fatalf("cooperative args must contain %v\ngot: %v", must, coop)
		}
	}
	// The image is the last positional before the held command's argument (infinity).
	if !argsContain(coop, "img:tag", "infinity") {
		t.Fatalf("image must precede the held command's arg, got: %v", coop)
	}

	// (2) CONFINED opt-in (confined=true, network="none"): the full old confinement —
	// --cap-drop ALL + --read-only + --network none (no egress).
	confined := containerRunArgs("nm-x", "/tmp/cid", "/work", "/host/clone", "/host/state", "img:tag", "none", true, nil, "linux/arm64")
	if got := flagValue(confined, "--network"); got != "none" {
		t.Fatalf("confined opt-in must pass --network none, got %q", got)
	}
	if !argsContain(confined, "--cap-drop", "ALL") || !argsContain(confined, "--read-only") {
		t.Fatalf("confined must add cap-drop ALL + read-only, got: %v", confined)
	}
	// /tmp stays writable even under the read-only rootfs.
	if !argsContain(confined, "--tmpfs", "/tmp") {
		t.Fatalf("confined must keep a writable /tmp, got: %v", confined)
	}

	// (3) NAMED network with the cooperative grain (confined=false): a non-none value is
	// passed through as --network <value>, and caps/rootfs stay relaxed (cooperative).
	named := containerRunArgs("nm-x", "/tmp/cid", "/work", "/host/clone", "/host/state", "img:tag", "my-net", false, nil, "linux/arm64")
	if got := flagValue(named, "--network"); got != "my-net" {
		t.Fatalf("a named network must pass through as --network <value>, got %q", got)
	}
	if argsContain(named, "--cap-drop", "ALL") || argsContain(named, "--read-only") {
		t.Fatalf("a cooperative grain with a named network must NOT add cap-drop/read-only, got: %v", named)
	}
	// (4) PLATFORM pin is rendered as --platform <value>; "" omits it (host default).
	if got := flagValue(coop, "--platform"); got != "linux/arm64" {
		t.Fatalf("--platform must be rendered, got %q", got)
	}
	if argsContain(containerRunArgs("n", "/c", "/w", "/cl", "/s", "img", "", false, nil, ""), "--platform") {
		t.Fatalf("empty platform must OMIT --platform")
	}
}

// TestNewContainerGrainConfinedDial: the master CONFINED toggle defaults OFF (the
// cooperative grain — writable rootfs, default caps, egress) and is turned ON by a
// truthy MAD_CONTAINER_CONFINED, which also implies network "none" UNLESS an
// explicit MAD_CONTAINER_NETWORK overrides it (no runtime needed).
func TestNewContainerGrainConfinedDial(t *testing.T) {
	repo := t.TempDir()
	// Default: not confined, cooperative network "".
	if g := newContainerGrain(repo); g.confined || g.network != "" {
		t.Fatalf("default must be cooperative (confined=false, network=\"\"), got confined=%v network=%q", g.confined, g.network)
	}
	// Truthy variants all enable confinement and imply network none.
	for _, v := range []string{"1", "true", "on", "yes", "TRUE", " Yes "} {
		t.Setenv("MAD_CONTAINER_CONFINED", v)
		g := newContainerGrain(repo)
		if !g.confined {
			t.Fatalf("MAD_CONTAINER_CONFINED=%q must enable confinement", v)
		}
		if g.network != "none" {
			t.Fatalf("confined with no network override must imply network none, got %q (CONFINED=%q)", g.network, v)
		}
	}
	// An explicit network override WINS even under confinement (egress is restored).
	t.Setenv("MAD_CONTAINER_CONFINED", "1")
	t.Setenv("MAD_CONTAINER_NETWORK", "my-net")
	if g := newContainerGrain(repo); !g.confined || g.network != "my-net" {
		t.Fatalf("explicit network override must win under confinement, got confined=%v network=%q", g.confined, g.network)
	}
	// Explicit "none" with no confinement still selects no-egress (the network knob is
	// independent of the master toggle).
	t.Setenv("MAD_CONTAINER_CONFINED", "")
	t.Setenv("MAD_CONTAINER_NETWORK", "none")
	if g := newContainerGrain(repo); g.confined || g.network != "none" {
		t.Fatalf("explicit network=none without CONFINED must give confined=false network=none, got confined=%v network=%q", g.confined, g.network)
	}
}

// TestNewContainerGrainNetworkDial: the network knob defaults to "" (the cooperative
// runtime-default network) and is configured by MAD_CONTAINER_NETWORK — "none"
// is the no-egress opt-in (no runtime needed).
func TestNewContainerGrainNetworkDial(t *testing.T) {
	repo := t.TempDir()
	if g := newContainerGrain(repo); g.network != "" {
		t.Fatalf("default network must be \"\" (runtime default = cooperative egress), got %q", g.network)
	}
	t.Setenv("MAD_CONTAINER_NETWORK", "none")
	if g := newContainerGrain(repo); g.network != "none" {
		t.Fatalf("MAD_CONTAINER_NETWORK=none must select the no-egress mode, got %q", g.network)
	}
}

// TestNewContainerGrainRejectsUnsafeKnobs: a flag-like / illegal MAD_CONTAINER_
// NETWORK or _IMAGE (arg-injection into `container run`) is REFUSED — the network
// FAILS to the cooperative default ("" = the runtime default network; confinement is
// never inferred from an unparseable value) and the image falls back to the default.
func TestNewContainerGrainRejectsUnsafeKnobs(t *testing.T) {
	repo := t.TempDir()
	// Unsafe network values fail to the cooperative default ("").
	for _, bad := range []string{"--cap-add", "--privileged", "-x", "a b", "a;b"} {
		t.Setenv("MAD_CONTAINER_NETWORK", bad)
		if g := newContainerGrain(repo); g.network != "" {
			t.Fatalf("unsafe network %q must fail to the cooperative default \"\", got %q", bad, g.network)
		}
	}
	// A legitimate named network is preserved.
	t.Setenv("MAD_CONTAINER_NETWORK", "my-net_1")
	if g := newContainerGrain(repo); g.network != "my-net_1" {
		t.Fatalf("a valid named network must be preserved, got %q", g.network)
	}
	// Unsafe image falls back to the default; a valid ref is preserved.
	t.Setenv("MAD_CONTAINER_NETWORK", "none")
	t.Setenv("MAD_CONTAINER_IMAGE", "--privileged")
	if g := newContainerGrain(repo); g.image != defaultContainerImage {
		t.Fatalf("unsafe image must fall back to default, got %q", g.image)
	}
	t.Setenv("MAD_CONTAINER_IMAGE", "alpine/git:latest")
	if g := newContainerGrain(repo); g.image != "alpine/git:latest" {
		t.Fatalf("a valid image ref must be preserved, got %q", g.image)
	}
}

// TestContainerGrainTeardownPreservesCloneOnHarvestFailure proves the DATA-SAFETY
// fix: if harvest fails (here, HostWorktree is not a real git clone), Teardown does
// NOT delete the clone — it PRESERVES it and surfaces an error — so the agent's
// commits (the sole copy) are recoverable rather than silently lost. No runtime
// needed (empty ContainerID skips the container CLI).
func TestContainerGrainTeardownPreservesCloneOnHarvestFailure(t *testing.T) {
	repo := initRepo(t)
	// A directory that EXISTS but is NOT a git repo → the harvest fetch fails.
	notAClone := filepath.Join(t.TempDir(), "broken-clone")
	if err := os.MkdirAll(notAClone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notAClone, "WORK.txt"), []byte("precious\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := newContainerGrain(repo)
	err := g.Teardown(Boundary{HostWorktree: notAClone, Branch: "nm/s-broken-zzzz"})
	if err == nil {
		t.Fatal("Teardown must surface the harvest failure (not silently succeed)")
	}
	// The clone is PRESERVED (not deleted) so the work can be recovered.
	if _, statErr := os.Stat(notAClone); statErr != nil {
		t.Fatalf("Teardown must PRESERVE the clone on harvest failure (it was deleted): %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(notAClone, "WORK.txt")); statErr != nil {
		t.Fatalf("the agent's work file must survive a harvest-failure teardown: %v", statErr)
	}
}

// TestContainerGrainTeardownHarvestsCloneBranch proves the PARITY fix WITHOUT a
// runtime: the container grain's Teardown harvests the clone's branch into the
// canonical repo (so a clean-exit/crash teardown does not lose unintegrated work,
// matching the worktree grain leaving its branch behind), then removes the clone.
// Driven with an empty ContainerID so no `container` CLI is needed.
func TestContainerGrainTeardownHarvestsCloneBranch(t *testing.T) {
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	repo := initRepo(t)

	const slug = "s-harvest-eeee"
	wt, err := worktree.CreateClone(repo, slug)
	if err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	// The agent commits in its clone (host-side here; identical to what the bind mount
	// exposes from inside the container).
	if err := os.WriteFile(filepath.Join(wt.Path, "agent.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt.Path, "add", "agent.txt")
	gitRun(t, wt.Path, "commit", "-q", "-m", "agent work")

	g := newContainerGrain(repo)
	if err := g.Teardown(Boundary{HostWorktree: wt.Path, Branch: wt.Branch}); err != nil {
		t.Fatalf("Teardown (harvest + remove): %v", err)
	}

	// HARVESTED: the canonical repo now has the branch with the agent's commit.
	out, err := exec.Command("git", "-C", repo, "show", wt.Branch+":agent.txt").CombinedOutput()
	if err != nil {
		t.Fatalf("Teardown must harvest the clone's branch into the canonical repo: %v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "work" {
		t.Fatalf("harvested branch must carry the agent's commit, got %q", out)
	}
	// And the clone dir is removed (a clone is not a registered worktree → os.RemoveAll).
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("Teardown must remove the clone dir, stat err=%v", err)
	}
	// Harvest must NEVER advance trunk: only refs/heads/<branch> was written.
	if err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "refs/heads/trunk").Run(); err == nil {
		t.Fatal("harvest must NOT create/advance a trunk ref")
	}
}

// TestCooperativeCredentialMounts proves the R7 credential forwarding: the
// cooperative grain mounts EXISTING agent credential dirs read-write into the
// container home (no re-auth), the CONFINED grain forwards NONE (the confinement
// property — host secrets never enter an untrusted container), the escape hatch
// disables it, a missing dir is skipped, and containerRunArgs renders the mounts.
func TestCooperativeCredentialMounts(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{".codex", ".claude"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	t.Setenv("MAD_CONTAINER_CREDENTIALS", "") // empty != off → forwarding ON

	// (1) COOPERATIVE: codex's ~/.codex is forwarded directly (portable auth.json).
	// Claude is NOT a direct dir mount (its on-disk creds are stale) — it is sourced via
	// claudeCredentialMount instead (covered by TestClaudeCredentialMount).
	coop := cooperativeCredentialMounts(home, false)
	if len(coop) != 1 || coop[0][0] != filepath.Join(home, ".codex") || coop[0][1] != containerHome()+"/.codex" {
		t.Fatalf("cooperative must forward exactly ~/.codex -> %s/.codex, got %v", containerHome(), coop)
	}

	// (2) CONTROL (security-critical) — CONFINED forwards NONE: host secrets must
	// never enter an untrusted container, so this flips on any regression.
	if got := cooperativeCredentialMounts(home, true); got != nil {
		t.Fatalf("confined must forward NO credentials, got %v", got)
	}

	// (3) CONTROL — the escape hatch disables forwarding even in cooperative mode.
	t.Setenv("MAD_CONTAINER_CREDENTIALS", "off")
	if got := cooperativeCredentialMounts(home, false); got != nil {
		t.Fatalf("MAD_CONTAINER_CREDENTIALS=off must forward nothing, got %v", got)
	}
	t.Setenv("MAD_CONTAINER_CREDENTIALS", "")

	// (4) CONTROL — only EXISTING dirs are forwarded (a missing one is skipped).
	home2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home2, ".codex"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	only := cooperativeCredentialMounts(home2, false)
	if len(only) != 1 || only[0][1] != containerHome()+"/.codex" {
		t.Fatalf("only existing dirs forwarded; want just .codex, got %v", only)
	}

	// (5) containerRunArgs RENDERS the cooperative cred mounts as --mount args, and
	// the confined run (which passes none) renders NONE — the run-arg-level control.
	credMount := "type=bind,source=" + filepath.Join(home, ".codex") + ",target=" + containerHome() + "/.codex"
	args := containerRunArgs("nm-x", "/tmp/cid", "/work", "/c", "/s", "img:tag", "", false, coop, "linux/arm64")
	if !argsContain(args, "--mount", credMount) {
		t.Fatalf("cooperative run args must render the credential mount, got %v", args)
	}
	conf := containerRunArgs("nm-x", "/tmp/cid", "/work", "/c", "/s", "img:tag", "none", true, cooperativeCredentialMounts(home, true), "linux/arm64")
	if argsContain(conf, "--mount", credMount) {
		t.Fatalf("confined run args must NOT render any credential mount, got %v", conf)
	}
}

// TestCredentialForwardingOff covers the escape-hatch parser: empty/unset and a
// recognized OFF token are handled SILENTLY (on / off respectively); an unrecognized
// non-empty value FAILS OPEN (forwarding stays ON, returns false) but must WARN —
// matching the sibling MAD_CONTAINER_IMAGE / MAD_CONTAINER_NETWORK knobs.
func TestCredentialForwardingOff(t *testing.T) {
	// captureLog runs fn with the global logger redirected and returns what it wrote.
	captureLog := func(fn func()) string {
		var buf bytes.Buffer
		prevOut, prevFlags := log.Writer(), log.Flags()
		log.SetOutput(&buf)
		log.SetFlags(0)
		defer func() {
			log.SetOutput(prevOut)
			log.SetFlags(prevFlags)
		}()
		fn()
		return buf.String()
	}

	// (1) recognized OFF tokens disable forwarding, SILENTLY (no warning needed).
	for _, tok := range []string{"off", "0", "false", "no", "OFF", " No "} {
		t.Setenv("MAD_CONTAINER_CREDENTIALS", tok)
		var got bool
		out := captureLog(func() { got = credentialForwardingOff() })
		if !got {
			t.Fatalf("token %q must DISABLE forwarding (return true), got false", tok)
		}
		if out != "" {
			t.Fatalf("token %q is recognized — must not warn, got %q", tok, out)
		}
	}

	// (2) empty/unset ⇒ cooperative default: forwarding ON, no warning.
	t.Setenv("MAD_CONTAINER_CREDENTIALS", "")
	var emptyGot bool
	if out := captureLog(func() { emptyGot = credentialForwardingOff() }); emptyGot || out != "" {
		t.Fatalf("empty value must keep forwarding ON silently; got off=%v log=%q", emptyGot, out)
	}

	// (3) CONTROL (the fix) — an UNRECOGNIZED non-empty value FAILS OPEN: forwarding
	// stays ON (returns false) AND a warning is emitted. Without the fix this is a
	// silent fail-open, so the log assertion flips red on regression.
	for _, bad := range []string{"disabled", "none", "never", "yes"} {
		t.Setenv("MAD_CONTAINER_CREDENTIALS", bad)
		var got bool
		out := captureLog(func() { got = credentialForwardingOff() })
		if got {
			t.Fatalf("unrecognized %q must keep forwarding ON (return false), got true", bad)
		}
		if !strings.Contains(out, "MAD_CONTAINER_CREDENTIALS") || !strings.Contains(out, bad) {
			t.Fatalf("unrecognized %q must WARN naming the env + value, got %q", bad, out)
		}
	}
}

// TestNormalizeClaudeCredentials: a wrapped {"claudeAiOauth":...} blob is written
// as-is; a bare inner object (has accessToken) is WRAPPED; foreign/invalid JSON errors.
func TestNormalizeClaudeCredentials(t *testing.T) {
	wrapped := []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"y"}}`)
	if got, err := normalizeClaudeCredentials(wrapped); err != nil || string(got) != string(wrapped) {
		t.Fatalf("wrapped: got %s err %v", got, err)
	}
	got, err := normalizeClaudeCredentials([]byte(`{"accessToken":"x","refreshToken":"y"}`))
	if err != nil {
		t.Fatalf("inner: %v", err)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(got, &m)
	if m["claudeAiOauth"] == nil {
		t.Fatalf("a bare inner object must be wrapped under claudeAiOauth, got %s", got)
	}
	if _, err := normalizeClaudeCredentials([]byte(`{"unrelated":1}`)); err == nil {
		t.Fatalf("foreign JSON must error")
	}
	if _, err := normalizeClaudeCredentials([]byte("not json")); err == nil {
		t.Fatalf("invalid JSON must error")
	}
}

// TestClaudeCredentialMount stubs the Keychain reader: cooperative ⇒ a FRESH
// .credentials.json is materialized (0600) + the /root/.claude mount returned; CONFINED
// ⇒ nothing (host secrets withheld — the security control); no live creds ⇒ nothing.
func TestClaudeCredentialMount(t *testing.T) {
	orig := keychainReader
	t.Cleanup(func() { keychainReader = orig })
	t.Setenv("MAD_CONTAINER_CREDENTIALS", "")

	keychainReader = func(string) ([]byte, bool) {
		return []byte(`{"claudeAiOauth":{"accessToken":"live","refreshToken":"r"}}`), true
	}
	src, target, ok := claudeCredentialMount("/no/home", t.TempDir(), false)
	if !ok || target != containerHome()+"/.claude" {
		t.Fatalf("cooperative: ok=%v target=%q", ok, target)
	}
	credFile := filepath.Join(src, ".credentials.json")
	b, err := os.ReadFile(credFile)
	if err != nil || !strings.Contains(string(b), "claudeAiOauth") {
		t.Fatalf("materialized creds missing/short: %v %s", err, b)
	}
	if fi, _ := os.Stat(credFile); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("creds file must be 0600")
	}

	// CONTROL (security): confined ⇒ NO claude creds materialized/mounted.
	if _, _, ok := claudeCredentialMount("/no/home", t.TempDir(), true); ok {
		t.Fatalf("confined must NOT forward claude creds")
	}
	// CONTROL: no Keychain item + no on-disk file ⇒ nothing.
	keychainReader = func(string) ([]byte, bool) { return nil, false }
	if _, _, ok := claudeCredentialMount("/no/home", t.TempDir(), false); ok {
		t.Fatalf("no live creds ⇒ no mount")
	}
}

// TestApiserverDownClassification covers the pure preflight classifier: "not
// running"/"not registered"/absent-running-token ⇒ down (fail-closed); a "running"
// token ⇒ up. ("not running" must be matched before the bare "running" substring.)
func TestApiserverDownClassification(t *testing.T) {
	for _, s := range []string{
		"apiserver is not running and not registered with launchd",
		"not registered",
		"", // no "running" token → fail-closed down
		"some unknown state",
	} {
		if !apiserverDown(s) {
			t.Errorf("apiserverDown(%q) = false, want true (down)", s)
		}
	}
	for _, s := range []string{"status running", "apiserver: running", "RUNNING"} {
		if apiserverDown(s) {
			t.Errorf("apiserverDown(%q) = true, want false (up)", s)
		}
	}
}

// TestContainerPlatform: the env override wins when safe, an unsafe override is
// ignored, and the default pins the host GOARCH.
func TestContainerPlatform(t *testing.T) {
	t.Setenv("MAD_CONTAINER_PLATFORM", "linux/amd64")
	if got := containerPlatform(); got != "linux/amd64" {
		t.Fatalf("env override: got %q", got)
	}
	t.Setenv("MAD_CONTAINER_PLATFORM", "--privileged") // unsafe / flag-like
	if got := containerPlatform(); got != "linux/"+runtime.GOARCH {
		t.Fatalf("unsafe override must fall back to host default, got %q", got)
	}
	t.Setenv("MAD_CONTAINER_PLATFORM", "")
	if got := containerPlatform(); got != "linux/"+runtime.GOARCH {
		t.Fatalf("default: got %q want linux/%s", got, runtime.GOARCH)
	}
}

// TestContainerHome: a safe absolute override wins; an UNSAFE value (relative,
// flag-like, or with illegal chars) falls back to /root; unset is /root. The
// fallback is the security-relevant control — an arg-injectable HOME must never
// become a mount target.
func TestContainerHome(t *testing.T) {
	// Env override honored for a safe absolute path.
	t.Setenv("MAD_CONTAINER_HOME", "/home/agent")
	if got := containerHome(); got != "/home/agent" {
		t.Fatalf("safe override: got %q want /home/agent", got)
	}
	// CONTROL — unsafe values must ALL fall back to /root (no arg-injection, no
	// relative path). Each row would slip through a naive os.Getenv passthrough.
	for _, bad := range []string{
		"relative/home",   // not absolute
		"--privileged",    // flag-like
		"/home/$(whoami)", // shell metachars
		"/home/a b",       // space
		"/home/a;rm",      // command separator
		"",                // empty
	} {
		t.Setenv("MAD_CONTAINER_HOME", bad)
		if got := containerHome(); got != "/root" {
			t.Fatalf("unsafe override %q must fall back to /root, got %q", bad, got)
		}
	}
	// Unset → /root.
	os.Unsetenv("MAD_CONTAINER_HOME")
	if got := containerHome(); got != "/root" {
		t.Fatalf("unset: got %q want /root", got)
	}
}

// TestClassifyAPIServerAction is the non-vacuous table for the pure preflight
// classifier. The DEGRADED row (status running, real op failing) is the whole point
// of the change: it must NOT be treated as healthy (a plain start is a no-op) — it
// must demand a full restart. The live stop/start/re-probe is not unit-testable.
func TestClassifyAPIServerAction(t *testing.T) {
	cases := []struct {
		name     string
		statusOK bool
		probeOK  bool
		want     apiserverAction
	}{
		{"running and real op works -> ok", true, true, apiserverOK},
		{"running but real op fails (degraded) -> restart", true, false, apiserverRestart},
		{"not running -> start", false, false, apiserverStart},
		{"status down, probe not consulted -> start", false, true, apiserverStart},
	}
	for _, c := range cases {
		if got := classify(c.statusOK, c.probeOK); got != c.want {
			t.Errorf("%s: classify(%v,%v) = %d, want %d", c.name, c.statusOK, c.probeOK, got, c.want)
		}
	}
	// NON-VACUOUS CONTROL: if classify IGNORED probeOK (the bug this guards), the
	// degraded case (T,F) would return apiserverOK and the runtime would never be
	// restarted — these two assertions flip red on exactly that regression.
	if classify(true, false) == apiserverOK {
		t.Fatal("degraded apiserver (status running, real op failing) must NOT classify OK")
	}
	if classify(true, false) == apiserverStart {
		t.Fatal("a degraded apiserver needs a full RESTART, not a no-op start")
	}
	// And a healthy apiserver must NOT be needlessly restarted (no flapping).
	if classify(true, true) != apiserverOK {
		t.Fatal("healthy apiserver must classify OK (no needless restart)")
	}
}
