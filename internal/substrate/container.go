package substrate

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/madhavhaldia/mad-substrate/internal/worktree"
)

// containerGrain is the T3 grain: it upgrades isolation from the v1 WORKTREE
// grain to a confined Linux container (Apple `container`) where the agent runs
// inside its OWN git clone + its OWN per-agent state — and NO OTHER host path. It
// makes the Inv-10 "grain dial" real.
//
// ISOLATION-BY-DEFAULT, CONFINEMENT IS OPT-IN. The DEFAULT container grain is
// COOPERATIVE: the agent gets a WRITABLE rootfs, the runtime's DEFAULT caps, and
// the runtime's DEFAULT network (NAT egress) — matching the default WORKTREE grain,
// which already gives full host FS+network to a cooperative agent. HOST ISOLATION
// is MOUNT-STRUCTURAL and holds in BOTH modes regardless: the host FS is simply NOT
// mounted into the container except the agent's own /work clone + its own state dir,
// so the agent cannot reach the host FS, the canonical trunk, or the daemon socket
// no matter what it writes in its ephemeral container. read-only/cap-drop only
// govern the container's OWN ephemeral rootfs and the in-container process caps —
// confinement EXTRAS aimed at a HOSTILE agent, NOT host isolation.
//
// The full CONFINEMENT bundle is the explicit OPT-IN MAD_CONTAINER_CONFINED=1
// (truthy: 1/true/on/yes): it adds --cap-drop ALL, a --read-only rootfs, AND implies
// network none (no egress) UNLESS MAD_CONTAINER_NETWORK overrides the network.
// This is a first-class capability for an UNTRUSTED agent: it strips capabilities,
// freezes the rootfs, and (via network none) makes the governance socket structurally
// unreachable while closing the NAT data-exfiltration channel. none-by-default was an
// inconsistency, not real protection — host isolation never depended on it.
//
// What it does on Provision:
//   - creates the agent's host workspace as a SELF-CONTAINED git CLONE (NOT a
//     linked worktree) off HEAD on branch nm/<session>, OUTSIDE the repo. A linked
//     worktree's .git is a file pointing back into the canonical repo (outside the
//     mount) and is unusable inside the container; a clone's objects live in the
//     mount, so the agent can commit (worktree.CreateClone). This CLOSES the
//     in-container git dev loop: in-container commits land on the host via the
//     bind mount, and `integrate --from <clone> <branch>` (or harvest-on-teardown)
//     brings the branch back to the canonical repo for a lease-serialized merge.
//   - starts a DETACHED container with that clone bind-mounted at /work and a hold
//     command keeping it alive (confinement flags applied per the mode above). The
//     launcher then `container exec`s the agent INTO the container at /work.
//
// NETWORK precedence: an EXPLICIT MAD_CONTAINER_NETWORK always WINS (any safe
// value — "none", a named network, or "default" — passed through as --network; the
// empty "" sentinel omits --network → the runtime's default egress). With NO explicit
// network override, MAD_CONTAINER_CONFINED=1 implies network none (the full old
// confinement), while the cooperative default omits --network entirely (NAT egress +
// DNS, internet reachable) — the 99% case (an agent that must `npm install`/`pip`/
// reach an API). The conformance network-confinement tier opts into the confined mode
// explicitly and stays green.
//
// WRITABLE SURFACES (so a real agent honoring TMPDIR/XDG can actually run even when
// CONFINED freezes the rootfs): the agent's INJECTED scratch/cache/state paths
// (buildEnv sets TMPDIR/XDG_CACHE_HOME/XDG_STATE_HOME + MAD_SCRATCH/CACHE/
// STATE to the per-agent HOST state dir) must resolve to a WRITABLE location. We
// bind-mount the agent's OWN per-agent host state dir into the container at its
// SAME host path (writable) so those exact paths resolve, and add --tmpfs /tmp so
// the conventional /tmp is writable too (unconditional — harmless in either mode).
// NO OTHER host path is mounted: the only writable host-backed surface is the agent's
// own /work and its own per-agent state dir — host FS confinement is MOUNT-STRUCTURAL
// and holds in both modes (a host sentinel outside these mounts stays unreachable).
// The per-agent state dir is the SAME path substrate.provisionState materializes
// (stateBase(repo)/slug), so the in-container TMPDIR/XDG/MAD_* paths and the
// host state dir coincide.
//
// CONCURRENCY (chafe C4): like the worktree grain conducts git, this grain
// conducts the `container` CLI, which manages its own daemon-side state; we
// SERIALIZE create/run/rm behind a mutex (and the worktree create/remove it
// wraps is itself not parallel-safe). The slow op is held under THIS grain's
// mutex only — never the substrate's live-map lock — so other sessions' port and
// state setup still proceed concurrently.
//
// COOPERATIVE PLANE (C5/C13 upgraded; the git dev loop CLOSES above, and the
// cooperative tail is now SHIPPED): the COOPERATIVE adapter reaching the daemon
// socket from INSIDE the container (lease/classify/integrator RPC) is solved by a
// token-authed exec-stdio RELAY. Apple `container` v1.0.0 has NO general
// host→container unix-socket forward (only --ssh), and bind-mounting the daemon
// socket gives "connection refused" across the VM boundary — so the launcher tunnels
// the daemon socket over the STDIO of a SECOND `container exec` of a static linux
// relay (cmd/mad-substrate-relay + internal/launcher/coop.go + internal/coop/frame.go).
// Each forwarded connection rebinds to the session via the in-container adapter's own
// token-authed session.attach (Inv 4), proven end to end by
// internal/launcher/coop_live_test.go. The relay ships EMBEDDED in the shipped binary
// (internal/coopembed, a -tags coopembed build) and AUTO-RESOLVES, so the cooperative
// plane is ON BY DEFAULT for the container grain; MAD_CONTAINER_RELAY overrides
// with an explicit host relay path. The remaining work is build/wiring (staging the
// embedded linux mad-substrate so the in-container cooperative layer is also present), not
// a missing transport. The HARD FLOOR (FS confinement + the integrator gate) already
// makes a container agent SAFE even when the plane is absent — it is a fail-soft
// coordination nicety, never a safety boundary. See contain.go.
type containerGrain struct {
	repo    string
	image   string
	network string // effective network: "" = runtime default network (egress); "none" = no egress; any other = named --network
	// confined is the master OPT-IN (MAD_CONTAINER_CONFINED truthy): when true the
	// container gets --cap-drop ALL + a --read-only rootfs (and, unless an explicit
	// MAD_CONTAINER_NETWORK overrides, network none). Default false = the
	// cooperative grain (default caps + writable rootfs + egress). Host isolation is
	// mount-structural and holds regardless of this flag.
	confined bool
	mu       sync.Mutex
}

// defaultContainerImage is cached on the verified host; override per deployment
// with MAD_CONTAINER_IMAGE.
const defaultContainerImage = "alpine:latest"

// defaultContainerNetwork is the COOPERATIVE default: the empty sentinel means
// "use the runtime's DEFAULT network" — the --network flag is OMITTED entirely, so
// the container gets NAT egress + DNS (internet reachable), matching the worktree
// grain's full host network. CONFINEMENT (no egress) is the opt-in:
// MAD_CONTAINER_NETWORK=none.
const defaultContainerNetwork = ""

// containerHome returns the conventional HOME inside the container, where an agent
// looks for its credential dirs (~/.codex, ~/.claude) and where a non-root container
// user's HOME lives. The default is "/root" (alpine / node:alpine run as root).
//
// It is OVERRIDABLE via MAD_CONTAINER_HOME to support a NON-ROOT container user
// (claude REFUSES --dangerously-skip-permissions as root, so the non-root path needs a
// HOME the user owns). Only a SAFE ABSOLUTE path is honored: it MUST start with '/' (so
// it can never be parsed as a flag) and contain only [A-Za-z0-9._/-]; any other value
// (relative, flag-like, illegal chars, empty) falls back to "/root".
func containerHome() string {
	if h := strings.TrimSpace(os.Getenv("MAD_CONTAINER_HOME")); isSafeContainerHome(h) {
		return h
	}
	return "/root"
}

// isSafeContainerHome reports whether an ENV-sourced HOME is a safe ABSOLUTE path: it
// MUST start with '/' (so it is never flag-like / arg-injectable) and contain only
// characters legal in a simple path ([A-Za-z0-9._/-]). Anything else is rejected so
// containerHome falls back to the safe "/root" default.
func isSafeContainerHome(s string) bool {
	if !strings.HasPrefix(s, "/") {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '/' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// knownCredentialDirs are the per-agent credential DIRECTORIES whose host copy is
// PORTABLE as-is — codex's ~/.codex (OAuth bearer tokens in auth.json, host-agnostic),
// mounted directly. Claude is deliberately NOT here: its on-disk
// ~/.claude/.credentials.json is frequently STALE (Claude Code refreshes tokens into
// the macOS Keychain, not the file), so it is sourced LIVE via claudeCredentialMount.
var knownCredentialDirs = []string{".codex"}

// credentialForwardingOff reports the cooperative-mode escape hatch
// MAD_CONTAINER_CREDENTIALS=off (off/0/false/no): forward NO host creds even in
// the cooperative grain. Shared by every credential-forwarding path.
func credentialForwardingOff() bool {
	raw := strings.TrimSpace(os.Getenv("MAD_CONTAINER_CREDENTIALS"))
	switch strings.ToLower(raw) {
	case "":
		return false // unset/empty ⇒ cooperative default: forwarding ON
	case "off", "0", "false", "no":
		return true
	}
	// A non-empty value we don't recognize as an OFF token (e.g. "disabled" /
	// "none" / "never"): fail OPEN — credential forwarding REMAINS ON — but WARN
	// rather than silently ignore, matching the sibling MAD_CONTAINER_IMAGE /
	// MAD_CONTAINER_NETWORK knobs which log + fall back on an unrecognized value.
	log.Printf("substrate: unrecognized MAD_CONTAINER_CREDENTIALS %q (expected one of off/0/false/no to disable); credential forwarding REMAINS ON", raw)
	return false
}

// cooperativeCredentialMounts returns the host→container bind-mount pairs that
// forward an agent's credentials into the container so it authenticates EXACTLY as
// on the host — the credential analogue of the cooperative-default egress network.
//
// COOPERATIVE-BY-DEFAULT, WITHHELD WHEN CONFINED. In the default (cooperative) grain
// each existing knownCredentialDir under hostHome is mounted READ-WRITE at the same
// name under containerHome (read-write so OAuth token refresh persists — matching the
// worktree grain, which already gives a cooperative agent full host-home access). When
// confined==true (MAD_CONTAINER_CONFINED=1, the UNTRUSTED-agent mode) NO
// credentials are forwarded — withholding host secrets is the whole point of
// confinement (so this never widens the confined attack surface). The escape hatch
// MAD_CONTAINER_CREDENTIALS=off (off/0/false/no) disables forwarding even in
// cooperative mode. hostHome is PASSED (not read here) so the set is unit-testable.
func cooperativeCredentialMounts(hostHome string, confined bool) [][2]string {
	if confined || hostHome == "" || credentialForwardingOff() {
		return nil // confined / opted-out: withhold host secrets (the confinement property)
	}
	var mounts [][2]string
	for _, d := range knownCredentialDirs {
		src := filepath.Join(hostHome, d)
		if fi, err := os.Stat(src); err == nil && fi.IsDir() {
			mounts = append(mounts, [2]string{src, containerHome() + "/" + d})
		}
	}
	return mounts
}

// claudeKeychainItem is the macOS login-Keychain generic-password SERVICE where
// Claude Code stores its LIVE OAuth credentials. The on-disk
// ~/.claude/.credentials.json is frequently STALE (Claude Code refreshes tokens into
// the Keychain, not the file), so a cooperative container sources the live creds here.
const claudeKeychainItem = "Claude Code-credentials"

// keychainReader reads a macOS login-Keychain generic-password VALUE by service name.
// A var so tests can stub it. ok=false when not macOS, `security` is missing, or the
// item is absent — the caller then forwards no claude creds (fail-soft, no regression;
// codex is unaffected). The VALUE is a secret and is NEVER logged.
var keychainReader = func(service string) ([]byte, bool) {
	if runtime.GOOS != "darwin" {
		return nil, false
	}
	path, err := exec.LookPath("security")
	if err != nil {
		return nil, false
	}
	out, err := exec.Command(path, "find-generic-password", "-s", service, "-w").Output()
	if err != nil {
		return nil, false
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return nil, false
	}
	return []byte(v), true
}

// normalizeClaudeCredentials returns the bytes to write as Claude Code's
// .credentials.json given a raw credential blob. The file shape is
// {"claudeAiOauth":{...}}; the Keychain value is normally already that shape, but if
// it is the bare inner object (has "accessToken") we WRAP it. Foreign/invalid JSON →
// error (never write garbage). Never logs the secret.
func normalizeClaudeCredentials(raw []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("claude credentials: not JSON")
	}
	if _, ok := m["claudeAiOauth"]; ok {
		return raw, nil // already the .credentials.json shape
	}
	if _, ok := m["accessToken"]; ok {
		return json.Marshal(map[string]json.RawMessage{"claudeAiOauth": json.RawMessage(raw)})
	}
	return nil, fmt.Errorf("claude credentials: unrecognized shape")
}

// liveClaudeCredentials returns the LIVE Claude Code credentials to materialize: the
// macOS Keychain value first (the source of truth on darwin), else the on-disk
// ~/.claude/.credentials.json (a non-macOS host where the file IS live). ok=false when
// neither yields usable creds. Never logs the secret.
func liveClaudeCredentials(hostHome string) ([]byte, bool) {
	if raw, ok := keychainReader(claudeKeychainItem); ok {
		if creds, err := normalizeClaudeCredentials(raw); err == nil {
			return creds, true
		}
	}
	if hostHome != "" {
		if b, err := os.ReadFile(filepath.Join(hostHome, ".claude", ".credentials.json")); err == nil {
			if creds, err := normalizeClaudeCredentials(b); err == nil {
				return creds, true
			}
		}
	}
	return nil, false
}

// claudeCredentialMount materializes a FRESH Claude Code .credentials.json (from the
// macOS Keychain — see liveClaudeCredentials) into a per-agent staging .claude dir
// under stateDir (0600) and returns the host→container mount pair that places it at
// containerHome/.claude inside the container, so an in-container Claude authenticates
// with NO re-login. ok=false (no error surfaced) when confined / opted out / no live
// creds — claude-in-container then just won't auth (no regression). The token VALUE is
// never logged; the staging file is 0600.
func claudeCredentialMount(hostHome, stateDir string, confined bool) (src, target string, ok bool) {
	if confined || credentialForwardingOff() {
		return "", "", false
	}
	creds, found := liveClaudeCredentials(hostHome)
	if !found {
		return "", "", false
	}
	claudeDir := filepath.Join(stateDir, "claude-home", ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		return "", "", false
	}
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), creds, 0o600); err != nil {
		return "", "", false
	}
	return claudeDir, containerHome() + "/.claude", true
}

// newContainerGrain builds the container grain over repoAbs, resolving the image
// from MAD_CONTAINER_IMAGE (default alpine:latest), the CONFINEMENT master
// toggle from MAD_CONTAINER_CONFINED (truthy = the opt-in --cap-drop ALL +
// --read-only bundle), and the effective network via resolveContainerNetwork (an
// explicit MAD_CONTAINER_NETWORK wins; else confined implies "none"; else the
// cooperative default "" = the runtime's default network = egress).
func newContainerGrain(repoAbs string) *containerGrain {
	img := strings.TrimSpace(os.Getenv("MAD_CONTAINER_IMAGE"))
	if img != "" && !isSafeContainerArg(img) {
		// A flag-like / illegal image would be parsed as a `container run` FLAG
		// (arg-injection) — refuse it and fall back to the safe default.
		log.Printf("substrate: ignoring unsafe MAD_CONTAINER_IMAGE %q (flag-like / illegal chars); using %q", img, defaultContainerImage)
		img = ""
	}
	if img == "" {
		img = defaultContainerImage
	}
	confined := isTruthyEnv("MAD_CONTAINER_CONFINED")
	net := resolveContainerNetwork(confined)
	return &containerGrain{repo: repoAbs, image: img, network: net, confined: confined}
}

// resolveContainerNetwork computes the EFFECTIVE --network value with the documented
// precedence: an EXPLICIT, safe MAD_CONTAINER_NETWORK always WINS (passed
// through verbatim — "none", a named net, "default", etc.; an unsafe/flag-like value
// is rejected and treated as unset, to the cooperative default, never inferred into a
// confinement value). With NO explicit override, a confined grain implies "none" (no
// egress); otherwise the cooperative default "" (the runtime's default network = egress).
func resolveContainerNetwork(confined bool) string {
	raw := strings.TrimSpace(os.Getenv("MAD_CONTAINER_NETWORK"))
	if raw != "" && !isSafeContainerArg(raw) {
		// A flag-like / illegal network value could inject `container run` flags
		// (e.g. "--cap-add SYS_ADMIN"). REJECT it and treat as unset rather than honor
		// an unsafe value — the network falls through to the confined/cooperative default.
		log.Printf("substrate: ignoring unsafe MAD_CONTAINER_NETWORK %q (flag-like / illegal chars); falling back to the default network for this mode", raw)
		raw = ""
	}
	if raw != "" {
		return raw // explicit override wins (incl. "none" / a named net / "default")
	}
	if confined {
		return "none" // confined with no explicit override ⇒ no egress
	}
	return defaultContainerNetwork // cooperative default ("" = runtime default network = egress)
}

// isTruthyEnv reports whether the named env var is set to a truthy value
// (case-insensitive, trimmed): 1/true/on/yes. Used for the MAD_CONTAINER_CONFINED
// master toggle (default false = the cooperative grain).
func isTruthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// isSafeContainerArg reports whether an ENV-sourced value is safe to pass as a
// `container run` argument (the image or the --network value). It must be NON-empty,
// must NOT start with '-' (so it can NEVER be parsed as a CLI FLAG — the arg-injection
// the review flagged: a value like "--cap-add SYS_ADMIN" must not slip past), and must
// contain only characters legal in image refs and network specs
// ([A-Za-z0-9._/:,=@-]). A value that fails this is rejected by newContainerGrain in
// favor of a safe default (the network falls back to the cooperative "" = the
// runtime's default network; confinement is the explicit "none" opt-in).
func isSafeContainerArg(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '/' || r == ':' || r == ',' || r == '=' || r == '@' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// containerPlatform is the platform passed to `container run` so the runtime never
// mis-selects a multi-arch-index variant (the riscv64-on-arm64 bug: a DEGRADED
// apiserver's platform resolution falls back to an arbitrary index variant, whose
// wrong-arch rootfs then dies instantly on exec). Pinned to the HOST GOARCH (Apple
// `container` runs only on Apple Silicon ⇒ in practice linux/arm64). Override with
// MAD_CONTAINER_PLATFORM (safe values only).
func containerPlatform() string {
	if p := strings.TrimSpace(os.Getenv("MAD_CONTAINER_PLATFORM")); p != "" && isSafeContainerArg(p) {
		return p
	}
	return "linux/" + runtime.GOARCH
}

// apiserverDown reports whether a `container system status` output indicates the
// apiserver is NOT up. "not running"/"not registered" both mean down — "not running"
// is matched explicitly because it CONTAINS "running" as a substring; absent any
// "running" token we also treat it as down (fail-closed).
func apiserverDown(status string) bool {
	s := strings.ToLower(status)
	if strings.Contains(s, "not running") || strings.Contains(s, "not registered") {
		return true
	}
	return !strings.Contains(s, "running")
}

// apiserverAction is the recovery action the apiserver preflight must take given the
// two health signals. It is the output of the PURE classify helper so the decision
// logic is unit-testable without a live runtime (the recovery itself is not).
type apiserverAction int

const (
	apiserverOK      apiserverAction = iota // running AND a real op works: nothing to do
	apiserverStart                          // not running: a plain `system start`
	apiserverRestart                        // running but DEGRADED (a real op failed): stop then start
)

// classify maps the two apiserver health signals to the recovery action. statusOK is
// "`container system status` reports running"; probeOK is "a REAL op (`container ls`)
// succeeded". A running-but-failing-probe apiserver is DEGRADED (the XPC "Connection
// invalid" case where status lies) and needs a full STOP+START, not a no-op start
// against an already-"running" daemon. When statusOK is false the probe is not
// consulted (the not-running path goes straight to start) — pure and total.
func classify(statusOK, probeOK bool) apiserverAction {
	switch {
	case statusOK && probeOK:
		return apiserverOK
	case statusOK && !probeOK:
		return apiserverRestart
	default:
		return apiserverStart
	}
}

// apiserverHealth probes both signals: status text AND, only when status claims
// running, a REAL op (`container ls`) that exercises the XPC channel a degraded
// apiserver silently breaks. Returns (statusOK, probeOK) for classify.
func apiserverHealth() (statusOK, probeOK bool) {
	out, err := runContainer("system", "status")
	statusOK = err == nil && !apiserverDown(out)
	if statusOK {
		_, perr := runContainer("ls")
		probeOK = perr == nil
	}
	return statusOK, probeOK
}

// ensureContainerAPIServer makes the Apple `container` apiserver HEALTHY before a
// run. The apiserver does NOT reliably auto-start (it can be left unregistered with
// launchd after a crash / login-session change); when it is down EVERY `container`
// call either hangs or fails with "XPC connection error: Connection invalid" — the
// single root cause of all observed container-grain flakiness. A SUBTLER failure: the
// apiserver reports RUNNING yet every real op throws XPC "Connection invalid" — a
// DEGRADED daemon a plain `system start` will not fix (start is a no-op against an
// already-"running" daemon). So we probe a REAL op (`container ls`) when status says
// running and, if it fails, do a full STOP+START to rebuild the XPC channel.
//
// FAIL-CLOSED: after recovery we re-probe to the SAME health bar (status up AND a real
// op works); if it is still not healthy we return an error so Provision fails with an
// actionable message rather than handing back a broken container. Auto-recovery is the
// default; disable it with MAD_CONTAINER_NO_AUTOSTART=1 (a down/degraded
// apiserver then just errors). The start carries its own --timeout so this is bounded.
func ensureContainerAPIServer() error {
	switch classify(apiserverHealth()) {
	case apiserverOK:
		return nil // running AND a real op works
	case apiserverStart:
		if isTruthyEnv("MAD_CONTAINER_NO_AUTOSTART") {
			return fmt.Errorf("container apiserver is not running (auto-start disabled via MAD_CONTAINER_NO_AUTOSTART); run `container system start`")
		}
		_, _ = runContainer("system", "start", "--disable-kernel-install", "--timeout", "60")
	case apiserverRestart:
		// DEGRADED: status says running but a real op failed (XPC Connection invalid).
		// A plain start is a no-op here, so STOP first, then START to rebuild the channel.
		if isTruthyEnv("MAD_CONTAINER_NO_AUTOSTART") {
			return fmt.Errorf("container apiserver is degraded — running but a real op fails (auto-restart disabled via MAD_CONTAINER_NO_AUTOSTART); run `container system stop && container system start`")
		}
		_, _ = runContainer("system", "stop")
		_, _ = runContainer("system", "start", "--disable-kernel-install", "--timeout", "60")
	}
	// Re-probe to the SAME bar after recovery; fail-closed if still unhealthy.
	if statusOK, probeOK := apiserverHealth(); classify(statusOK, probeOK) != apiserverOK {
		return fmt.Errorf("container apiserver not healthy after recovery: status-ok=%v probe-ok=%v", statusOK, probeOK)
	}
	return nil
}

// containerIsRunning reports whether a container named name appears in the runtime's
// RUNNING list (`container ls` lists running containers). A post-run check: a
// wrong-arch / broken image dies INSTANTLY, so it is absent here even though `run`
// "succeeded" — catching it lets Provision fail with a clear message instead of the
// launcher's later misleading "container is not running" on exec. Best-effort: an
// `ls` error returns true (don't invent a new failure mode over a list quirk — the
// --platform pin is the real fix; this is defense in depth).
func containerIsRunning(name string) bool {
	out, err := runContainer("ls")
	if err != nil {
		return true
	}
	return strings.Contains(out, name)
}

func (g *containerGrain) Name() string { return "container" }

// containerWorkMount is where the agent's host worktree is bind-mounted inside
// the container and the cwd reported to the agent.
const containerWorkMount = "/work"

// Provision creates the host CLONE and starts a confined container with only that
// clone (and the agent's per-agent state) mounted. The returned Boundary reports
// the IN-CONTAINER cwd (/work) but carries the host clone path (for the
// disjoint-from-repo check, harvest, and teardown) and the container id (for
// `container exec`).
func (g *containerGrain) Provision(slug string) (Boundary, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// (preflight) the apiserver must be HEALTHY or every `container` op hangs / throws
	// XPC errors — bring it up (fail-closed) BEFORE we create any state to roll back.
	if err := ensureContainerAPIServer(); err != nil {
		return Boundary{}, err
	}

	// (a) a SELF-CONTAINED clone (NOT a linked worktree): the agent commits inside
	// the container whose ONLY mount is this dir, so its .git must be complete here.
	wt, err := worktree.CreateClone(g.repo, slug)
	if err != nil {
		return Boundary{}, err
	}

	name := "nm-" + slug

	// (a.1) the agent's per-agent host STATE dir — the SAME path
	// substrate.provisionState materializes (stateBase(repo)/slug). We create it
	// HERE (idempotent; provisionState's later MkdirAll is also idempotent) so it
	// EXISTS to be bind-mounted into the container at its identical host path,
	// giving the agent a writable surface that MATCHES the injected TMPDIR/XDG/
	// MAD_* env without exposing any other host path.
	stateRoot := filepath.Join(stateBase(g.repo), slug)
	if mkErr := os.MkdirAll(stateRoot, 0o700); mkErr != nil {
		_ = os.RemoveAll(wt.Path)
		return Boundary{}, fmt.Errorf("container: per-agent state dir: %w", mkErr)
	}

	// (b) a detached, confined container with ONLY the clone + state mounted. The
	// --cidfile gives us a deterministic id capture; the hold command keeps the
	// container alive so the launcher can exec into it. A best-effort rm of any
	// stale same-named container first makes re-provision after a crash idempotent.
	rmContainer(name) // best-effort prune of a stale same-named container

	cidFile, err := os.CreateTemp("", "nm-cid-*")
	if err != nil {
		_ = os.RemoveAll(wt.Path)
		return Boundary{}, fmt.Errorf("container: cidfile: %w", err)
	}
	cidPath := cidFile.Name()
	_ = cidFile.Close()
	// `container run --cidfile` requires the file not to pre-exist.
	_ = os.Remove(cidPath)
	defer os.Remove(cidPath)

	// COOPERATIVE CREDENTIAL FORWARDING (R7): in the cooperative grain forward the
	// agent's host credential dirs so the in-container agent authenticates with NO
	// re-login; EMPTY when confined (host secrets stay out of an untrusted container).
	homeDir, _ := os.UserHomeDir()
	credMounts := cooperativeCredentialMounts(homeDir, g.confined)
	// Claude Code: its LIVE creds live in the macOS Keychain (the on-disk
	// ~/.claude/.credentials.json is often stale), so source them and mount a fresh
	// per-agent .claude — unlike codex above, a plain dir mount would carry stale creds.
	if src, tgt, ok := claudeCredentialMount(homeDir, stateRoot, g.confined); ok {
		credMounts = append(credMounts, [2]string{src, tgt})
	}
	args := containerRunArgs(name, cidPath, containerWorkMount, wt.Path, stateRoot, g.image, g.network, g.confined, credMounts, containerPlatform())
	if out, runErr := runContainer(args...); runErr != nil {
		// Roll back the clone (state dir is reclaimed by substrate rollback /
		// teardown) so a failed run leaks nothing.
		rmContainer(name)
		_ = os.RemoveAll(wt.Path)
		return Boundary{}, fmt.Errorf("container run: %w: %s", runErr, out)
	}

	cid, err := readCID(cidPath)
	if err != nil {
		rmContainer(name)
		_ = os.RemoveAll(wt.Path)
		return Boundary{}, fmt.Errorf("container: capture id: %w", err)
	}

	// (post-run) a wrong-arch / broken image dies INSTANTLY; catch that here so we
	// fail with a clear message instead of the launcher's later "container is not
	// running" on exec. The --platform pin should prevent it; this is defense.
	if !containerIsRunning(name) {
		logs, _ := runContainer("logs", cid)
		rmContainer(name)
		_ = os.RemoveAll(wt.Path)
		return Boundary{}, fmt.Errorf("container %q exited immediately after run (image/platform mismatch?): %s", name, strings.TrimSpace(logs))
	}

	return Boundary{
		Cwd:          containerWorkMount, // the agent's cwd is inside the container
		Branch:       wt.Branch,
		HostWorktree: wt.Path,
		ContainerID:  cid,
	}, nil
}

// containerRunArgs builds the `container run` argv for a boundary. It is a PURE
// function (no side effects) so the flag set is unit-testable WITHOUT a runtime: the
// worktree+state bind-mounts, an UNCONDITIONAL writable /tmp, the NETWORK, and — only
// when confined — --cap-drop ALL + a --read-only rootfs.
//
// confined==false (the COOPERATIVE default) omits --cap-drop / --read-only entirely →
// the container gets the runtime's DEFAULT caps and a WRITABLE rootfs (a cooperative
// agent can install global tools / write caches outside /work). confined==true adds
// the confinement extras for an UNTRUSTED agent. Host isolation does NOT depend on
// either flag — it is the MOUNT STRUCTURE (only /work + the agent's state dir are
// bind-mounted), so a host sentinel outside those mounts is unreachable in both modes.
//
// network=="" OMITS --network → the runtime's DEFAULT network (NAT egress + DNS);
// network=="none" passes `--network none` = NO egress; any other value passes
// `--network <network>` = a named network. (Network precedence — explicit override vs
// confined-implies-none — is resolved upstream in resolveContainerNetwork.)
func containerRunArgs(name, cidPath, workMount, clonePath, stateRoot, image, network string, confined bool, credMounts [][2]string, platform string) []string {
	args := []string{
		"run", "-d",
		"--name", name,
		"--cidfile", cidPath,
		"--mount", "type=bind,source=" + clonePath + ",target=" + workMount,
		// The agent's OWN per-agent state dir, writable, at its IDENTICAL host path
		// so the injected TMPDIR/XDG/MAD_* env resolves to a writable surface.
		"--mount", "type=bind,source=" + stateRoot + ",target=" + stateRoot,
		"-w", workMount,
	}
	// PLATFORM pin (reliability): force the host arch so the runtime never mis-selects
	// a multi-arch-index variant (e.g. riscv64) that would die instantly on an arm64
	// host. "" omits it (the runtime's host-match default). See containerPlatform.
	if platform != "" {
		args = append(args, "--platform", platform)
	}
	// COOPERATIVE CREDENTIAL FORWARDING (R7): mount the agent's host credential dirs
	// (computed by cooperativeCredentialMounts — EMPTY when confined or opted out)
	// read-write so the in-container agent authenticates with NO re-login. The
	// credential analogue of the cooperative-default egress network; the confined
	// grain passes none, so host secrets never enter an untrusted container.
	for _, m := range credMounts {
		args = append(args, "--mount", "type=bind,source="+m[0]+",target="+m[1])
	}
	// CONFINEMENT EXTRAS (opt-in only): drop all caps + freeze the rootfs. These govern
	// the container's OWN ephemeral rootfs + in-container caps, NOT host isolation (which
	// is mount-structural). The cooperative default omits both → default caps + writable
	// rootfs, matching the worktree grain's cooperative posture.
	if confined {
		args = append(args, "--cap-drop", "ALL", "--read-only")
	}
	// The network: "" (cooperative default) OMITS --network → the runtime's default
	// network (egress); "none" = no netns interface/route (confined opt-in); any other
	// value = the named egress network.
	if network != "" {
		args = append(args, "--network", network)
	}
	args = append(args,
		// A writable /tmp (unconditional — harmless under either mode; the conventional
		// /tmp must be writable even when a read-only rootfs is in force).
		"--tmpfs", "/tmp",
		// HOLD the container alive with `sleep infinity`. --entrypoint OVERRIDES the
		// image's own ENTRYPOINT (a real agent image often sets one, e.g. alpine/git's
		// is `git` — without this override the hold command becomes `git sleep
		// infinity` and the container exits immediately). The launcher's `container
		// exec` runs the agent directly and is unaffected by the entrypoint.
		"--entrypoint", "sleep",
		image,
		"infinity",
	)
	return args
}

// Teardown removes the container (idempotent — a missing container is fine), then
// HARVESTS the clone's branch into the canonical repo, then removes the clone.
// Never leaks a container.
func (g *containerGrain) Teardown(b Boundary) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	var firstErr error
	if b.ContainerID != "" {
		if out, err := runContainer("rm", "-f", b.ContainerID); err != nil && !isNotFound(out) {
			firstErr = fmt.Errorf("container rm: %w: %s", err, out)
		}
	}
	if b.HostWorktree != "" {
		// PARITY with the worktree grain (whose Remove leaves the branch ref intact in
		// the canonical repo so unintegrated commits are not lost): harvest the clone's
		// branch into the canonical repo BEFORE deleting the clone, so a clean-exit /
		// crash teardown under `launch` preserves the agent's work as nm/<slug> (to
		// integrate later). NEVER advances trunk — see harvestBranch.
		//
		// DATA SAFETY: the clone is the SOLE copy of the agent's commits. If harvest
		// FAILS we must NOT delete the clone (that would silently lose the work) — we
		// PRESERVE it for recovery and surface the failure (a log line + the returned
		// error name where the work is). On the normal path harvest succeeds, so an
		// already-integrated boundary is still cleaned up (no leak).
		if hErr := g.harvestBranch(b.HostWorktree, b.Branch); hErr != nil {
			log.Printf("substrate: container teardown: HARVEST FAILED for %q — PRESERVING the clone at %q so its unintegrated commits are not lost: %v", b.Branch, b.HostWorktree, hErr)
			if firstErr == nil {
				firstErr = hErr
			}
			return firstErr // preserve the clone; do NOT RemoveAll
		}
		if err := os.RemoveAll(b.HostWorktree); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ReclaimOrphan tears down a container boundary the grain can RECONSTRUCT from
// the session SLUG alone — used by substrate.Teardown on a live-map MISS (a
// daemon restart reattaching from the durable lease ledger, where the in-memory
// live map is empty, so no Boundary survives). The container NAME is deterministic
// ("nm-"+slug, exactly as Provision mints it) and the clone path is derivable
// (worktree.Path), so a restarted daemon can free a leaked, clone-mounted
// container that the bare-return-nil Teardown path would otherwise leak forever.
// Best-effort + idempotent: an already-gone container/clone is benign.
func (g *containerGrain) ReclaimOrphan(slug string) error {
	g.mu.Lock()
	name := "nm-" + slug
	if out, err := runContainer("rm", "-f", name); err != nil && !isNotFound(out) {
		// Surface a genuine rm failure, but still try to reclaim the clone below.
		g.mu.Unlock()
		// Reconstruct + remove the clone even though the container rm errored, so we
		// reclaim as much as possible; return the container error as primary.
		_ = g.reclaimClone(slug)
		return fmt.Errorf("container reclaim rm %q: %w: %s", name, err, out)
	}
	g.mu.Unlock()
	return g.reclaimClone(slug)
}

// reclaimClone harvests then removes the reconstructed clone dir for slug
// (idempotent), the clone half of ReclaimOrphan. A clone is a plain directory (NOT
// a registered git worktree), so removal is os.RemoveAll — not `git worktree
// remove`. Harvest is best-effort so a crashed agent's committed work survives.
func (g *containerGrain) reclaimClone(slug string) error {
	path, err := worktree.Path(g.repo, slug)
	if err != nil || path == "" {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return nil // already reclaimed (or never created) — benign
	}
	// Preserve the orphan clone if harvest fails (its commits are the sole copy) —
	// same data-safety contract as Teardown.
	if hErr := g.harvestBranch(path, "nm/"+slug); hErr != nil {
		log.Printf("substrate: container reclaim: HARVEST FAILED for orphan %q — PRESERVING the clone at %q so its unintegrated commits are not lost: %v", "nm/"+slug, path, hErr)
		return hErr // preserve the orphan clone for recovery
	}
	if rmErr := os.RemoveAll(path); rmErr != nil {
		return rmErr
	}
	return nil
}

// harvestBranch fetches the clone's branch into the CANONICAL repo so the agent's
// (possibly unintegrated) commits survive teardown as a local branch ref — the
// container-grain analogue of the worktree grain leaving its branch behind. It
// writes ONLY refs/heads/<branch> (==nm/<slug>, a grain-controlled name) and NEVER
// advances trunk. MUST be called with g.mu held (it conducts git against g.repo). A
// later `integrate --from <clone>` would harvest the same ref; both are idempotent
// (+<branch>:<branch> force-updates).
//
// It returns an error ONLY when a genuine fetch FAILED (so the caller can PRESERVE
// the clone rather than silently lose the work); an empty arg or an absent clone is
// "nothing to harvest" (nil). A successful harvest of an empty/base-only branch is
// also nil.
func (g *containerGrain) harvestBranch(clonePath, branch string) error {
	if clonePath == "" || branch == "" {
		return nil
	}
	if _, err := os.Stat(clonePath); err != nil {
		return nil // no clone on disk — nothing to harvest
	}
	if out, err := harvestGit(g.repo, "fetch", "--no-tags", "--quiet", clonePath, "+"+branch+":"+branch); err != nil {
		return fmt.Errorf("harvest %s from %s: %w: %s", branch, clonePath, err, out)
	}
	return nil
}

// harvestGit conducts `git -C <repo> ...` for harvestBranch (the worktree package's
// git conductor is unexported; this is the container grain's own minimal one).
func harvestGit(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ExecGate runs a manifest gate command INSIDE a boundary container via
// `container exec <id> sh -c <gate>` — the convergent-plane analogue of the
// launcher's `container exec` agent-launch path, used by the conductor's GateRunner
// seam so a CONTAINER-grain gate validates the boundary where the agent's deps
// actually live (inside the container) instead of on the host clone.
//
// The GATE is passed as a SINGLE argument to `sh -c` (never host-shell-split): the
// in-container /bin/sh interprets it, exactly as the host worktree-grain gate does
// (`sh -c <gate>` in BoundaryDir). The container id is positional; it is guarded
// against arg-injection (a flag-like id that could be parsed as a `container exec`
// flag is rejected) — the id is grain-minted (Provision's cidfile), but the guard
// is defense in depth.
//
// It returns the gate's EXIT CODE (resolved from the process state), its combined
// output (so the caller can surface it — `container exec` does not stream live the
// way a host PTY does), and the os/exec error. The conductor treats a non-zero exit
// OR a non-nil error as StatusGateFailed (NOT merged) — identical to the host gate,
// whose `gc.Run()` likewise conflates a non-zero exit and a start failure. cgo-free:
// the `container` CLI is conducted via exec.Command, like git.
func ExecGate(containerID, gate string) (exitCode int, output []byte, err error) {
	if containerID == "" {
		return -1, nil, fmt.Errorf("container exec gate: empty container id")
	}
	if strings.HasPrefix(containerID, "-") {
		// A flag-like id would be parsed as a `container exec` FLAG (arg-injection).
		return -1, nil, fmt.Errorf("container exec gate: unsafe container id %q", containerID)
	}
	cmd := exec.Command("container", "exec", containerID, "sh", "-c", gate)
	out, runErr := cmd.CombinedOutput()
	code := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return code, out, runErr
}

// runContainer conducts the `container` CLI and returns its combined output.
func runContainer(args ...string) (string, error) {
	cmd := exec.Command("container", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// rmContainer force-removes a container by name/id, best-effort (rollback /
// stale-prune paths where any error is benign).
func rmContainer(id string) {
	_, _ = runContainer("rm", "-f", id)
}

// readCID reads and trims the container id the runtime wrote to the cidfile.
func readCID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	cid := strings.TrimSpace(string(b))
	if cid == "" {
		return "", fmt.Errorf("empty cidfile %q", path)
	}
	return cid, nil
}

// isNotFound reports whether a `container rm` failure is the benign
// already-gone case, so Teardown stays idempotent.
func isNotFound(out string) bool {
	o := strings.ToLower(out)
	return strings.Contains(o, "not found") || strings.Contains(o, "no such") || strings.Contains(o, "does not exist")
}
