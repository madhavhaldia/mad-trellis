// Package conformance is project 10a — the executable safety authority and the
// SOLE definer of self-hosting day (docs/0003 §10a, docs/0004 §10a). It boots a
// REAL governed scenario through the PUBLIC daemon contract + CLI ONLY and exits
// non-zero on ANY safety-clause failure. It owns no single invariant; it owns the
// CONJUNCTION of the safety property (GROUNDING L134-144) — (a) forkable
// isolation + no coordination channel, (b) no convergent write without an
// exclusive lease AND validated integration, (c) no singular effect without a
// grant — plus the escape-resistance, no-dispatch, joint-2b no-LLM-in-lock-path,
// the Inv-12 closed-loop guarantee, and the E2E acceptance gate.
//
// THE CARDINAL RULE — BLACK BOX THROUGH THE PUBLIC SURFACE ONLY. The harness
// asserts via the public daemon contract (the CLI + observable state: git refs,
// filesystem visibility, file contents, process exit codes, RPC verdicts from
// PUBLIC methods). It NEVER imports internal/{lease,substrate,integrator,singular,
// liveness,manifest,daemon,app,watch} to reach into or assert on internal state.
// The ONLY internal packages it may import are internal/rpcclient (the wire
// client — it speaks the frozen public protocol, same as any external client) and
// internal/protocol (envelope/taxonomy types). readonly_imports_test.go enforces
// this as a ship-time grep test.
package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
)

// ----------------------------------------------------------------------------
// Check / Result — the unit a probe-writer (phase 2) implements and registers.
// ----------------------------------------------------------------------------

// Check is one safety probe. Run drives the public surface and returns a Result.
// Control INJECTS the violation the Run path is meant to catch and asserts the
// check would flip to FAIL — proving the assertion is non-vacuous (every negative
// carries a positive control that genuinely goes RED). A Control that returns a
// nil error means "the check correctly went RED under the injected violation".
type Check interface {
	// ID is a short stable identifier ("forkable-isolation", "trunk-lease-gated").
	ID() string
	// Clause is the 0003 invariant-clause string this check proves.
	Clause() string
	// OwnerProject is the owning project id from the 0003 clause map.
	OwnerProject() string
	// Run executes the probe black-box and returns its Result.
	Run(s *Scratch) Result
	// Control injects the violation and asserts the check WOULD go RED. It returns
	// a non-nil error iff the check FAILED to detect the injected violation (i.e.
	// the assertion is vacuous). A check with no meaningful negative may return nil.
	Control(s *Scratch) error
}

// Result is the verdict of one Check.Run.
type Result struct {
	ID           string // the check id
	Clause       string // the 0003 clause proved
	OwnerProject string // the owning project id
	Pass         bool   // the safety property held
	Detail       string // human-readable evidence (why it passed / how it failed)
	// Skipped marks a check that could not be EVALUATED in this environment (e.g. the
	// container runtime is unavailable on a CI host). A skipped check does NOT redden
	// the gate (Pass stays true so the AND-composition is not failed by a missing
	// runtime) but it is rendered DISTINCTLY ([SKIP] + reason) so it is never a silent
	// green — the auditor sees exactly which structural assertion went un-evaluated and
	// why. The worktree-grain probes never skip; only the new container-grain
	// confinement tier does, when `container` is not on PATH / not reachable.
	Skipped bool
}

// pass / fail build a Result from a Check, so a probe never has to restate its
// own id/clause/owner.
func pass(c Check, format string, a ...any) Result {
	return Result{ID: c.ID(), Clause: c.Clause(), OwnerProject: c.OwnerProject(), Pass: true, Detail: fmt.Sprintf(format, a...)}
}

func fail(c Check, format string, a ...any) Result {
	return Result{ID: c.ID(), Clause: c.Clause(), OwnerProject: c.OwnerProject(), Pass: false, Detail: fmt.Sprintf(format, a...)}
}

// skip builds a SKIPPED Result: the check could not be evaluated in this
// environment (its detail must NAME the reason). Pass stays true so a missing
// runtime never reddens the AND-composed gate, but Skipped renders it distinctly —
// never a silent green. Used ONLY by the container-grain confinement tier when the
// `container` runtime is unavailable (a runtime-less CI host).
func skip(c Check, format string, a ...any) Result {
	return Result{ID: c.ID(), Clause: c.Clause(), OwnerProject: c.OwnerProject(), Pass: true, Skipped: true, Detail: "SKIPPED: " + fmt.Sprintf(format, a...)}
}

// ----------------------------------------------------------------------------
// Check registry — phase 2 extends the gate by RegisterCheck-ing more probes.
// ----------------------------------------------------------------------------

// registered is the ordered set of checks RunGate composes. Phase 2 adds probes
// by calling RegisterCheck from an init() in its own check file.
var registered []Check

// RegisterCheck adds a Check to the gate. Called from each check file's init().
// Duplicate ids panic (a probe-writer mistake surfaces at load, never silently).
func RegisterCheck(c Check) {
	for _, existing := range registered {
		if existing.ID() == c.ID() {
			panic("conformance: duplicate check id " + c.ID())
		}
	}
	registered = append(registered, c)
}

// Checks returns the registered checks in registration order (read-only view).
func Checks() []Check { return append([]Check(nil), registered...) }

// ----------------------------------------------------------------------------
// Scratch — the hermetic runtime: its OWN daemon on a SCRATCH runtime dir that
// NEVER touches ~/.mad-substrate or any already-running daemon.
// ----------------------------------------------------------------------------

// Scratch is one fully-isolated mad-substrate world: a scratch runtime dir (socket +
// ledger + trunk.git), a scratch governed git repo (the daemon's cwd / RepoRoot),
// scratch worktree + state dirs, and a single daemon process bound to a SHORT
// /tmp socket (macOS unix-socket path limit). Every spawned process — the daemon
// and every CLI call — gets MAD_RUNTIME_DIR / MAD_WORKTREE_DIR /
// MAD_STATE_DIR pointed at the scratch and --socket passed explicitly, so
// the harness is hermetic by construction.
type Scratch struct {
	Binary   string // the resolved mad-substrate binary path
	Root     string // the scratch runtime dir (socket + ledger.db + trunk.git live here)
	Socket   string // the short /tmp daemon socket path
	RepoDir  string // the governed git repo (the daemon's cwd / RepoRoot)
	WtDir    string // MAD_WORKTREE_DIR
	StateDir string // MAD_STATE_DIR
	BareDir  string // the mediated bare trunk repo (<Root>/trunk.git, created by the daemon)

	// grain is the substrate grain the scratch daemon is booted with. Empty (the
	// default) keeps the v1 WORKTREE grain — the entire existing gate is unchanged.
	// A container-grain confinement check calls UseContainerGrain to rebuild the
	// daemon under MAD_GRAIN=container; the daemon's substrate then provisions
	// CONFINED container boundaries (compose.go reads the env via substrate.New's dial).
	grain string

	// containerNetwork pins MAD_CONTAINER_NETWORK for the scratch daemon. Empty
	// (the default) lets the container grain derive the network: the cooperative default
	// (egress) when not confined, or "none" when confined. A non-empty value is an EXPLICIT
	// override that WINS over the confined-implies-none default — kept so a check can still
	// pin a specific network independent of the master toggle.
	containerNetwork string

	// containerConfined pins MAD_CONTAINER_CONFINED=1 for the scratch daemon — the
	// master CONFINEMENT opt-in (--cap-drop ALL + --read-only rootfs + network none unless
	// containerNetwork overrides). Empty (the default) keeps the COOPERATIVE container
	// grain (default caps + writable rootfs + egress). The confinement tier sets it via
	// UseConfinedContainerGrain so its read-only / no-egress assertions hold WHEN CHOSEN
	// (confinement is no longer the default).
	containerConfined bool

	daemon    *exec.Cmd
	daemonLog *bytes.Buffer
	cleanup   []func()
}

// NewScratch mints a hermetic world and STARTS its daemon, keying off socket
// readiness (NOT a sleep). binary is the mad-substrate binary to drive (the conform
// CLI passes os.Executable(); the tagged test go-builds one to a temp dir). The
// returned Scratch must be Closed (kills the daemon, removes scratch dirs).
func NewScratch(binary string) (*Scratch, error) {
	if binary == "" {
		return nil, fmt.Errorf("conformance: a mad-substrate binary path is required")
	}
	abs, err := filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("conformance: binary %q: %w", abs, err)
	}

	root, err := os.MkdirTemp("", "nmc-root-")
	if err != nil {
		return nil, err
	}
	s := &Scratch{
		Binary:   abs,
		Root:     root,
		RepoDir:  filepath.Join(root, "repo"),
		WtDir:    filepath.Join(root, "wt"),
		StateDir: filepath.Join(root, "state"),
	}
	s.cleanup = append(s.cleanup, func() { _ = os.RemoveAll(root) })

	// SHORT socket on /tmp — the macOS unix-socket path limit (~104 chars) is well
	// below a t.TempDir() path, so the socket lives in its own short temp dir. The
	// daemon derives ledger.db AND the mediated trunk.git from dirname(socket), so
	// the mediated bare repo lives NEXT TO the socket, not under Root.
	sockDir, err := os.MkdirTemp("/tmp", "nmc-")
	if err != nil {
		s.Close()
		return nil, err
	}
	s.cleanup = append(s.cleanup, func() { _ = os.RemoveAll(sockDir) })
	s.Socket = filepath.Join(sockDir, "d.sock")
	s.BareDir = filepath.Join(sockDir, "trunk.git") // daemon: dirname(socket)/trunk.git

	if err := s.initGovernedRepo(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.startDaemon(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// env returns the hermetic environment every spawned process inherits — the
// runtime/worktree/state dirs pointed at the scratch so nothing touches the real
// ~/.mad-substrate. GIT_CONFIG_GLOBAL/SYSTEM are nulled so a developer's global git
// config can never perturb a probe (mirrors integrator isolateGitGlobal).
func (s *Scratch) env() []string {
	e := append(os.Environ(),
		"MAD_RUNTIME_DIR="+s.Root,
		"MAD_WORKTREE_DIR="+s.WtDir,
		"MAD_STATE_DIR="+s.StateDir,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
	)
	// The grain DIAL (Inv 10-grainswap): empty → the daemon's default worktree grain
	// (zero behavior change). A container-grain check sets s.grain, so the scratch
	// daemon's substrate provisions confined container boundaries.
	if s.grain != "" {
		e = append(e, "MAD_GRAIN="+s.grain)
	}
	// The master CONFINEMENT opt-in: when set (the confinement tier sets it via
	// UseConfinedContainerGrain) the scratch daemon's container grain applies --cap-drop
	// ALL + a --read-only rootfs and implies network none. Unset → the cooperative grain.
	if s.containerConfined {
		e = append(e, "MAD_CONTAINER_CONFINED=1")
	}
	// An EXPLICIT network override (independent of the master toggle): when set it pins
	// MAD_CONTAINER_NETWORK and WINS over the confined-implies-none default. Unset →
	// the grain derives the network (cooperative egress, or none when confined).
	if s.containerNetwork != "" {
		e = append(e, "MAD_CONTAINER_NETWORK="+s.containerNetwork)
	}
	return e
}

// daemonEnv is the environment for the scratch DAEMON subprocess: the shared env
// plus the parent-death opt-in. An interrupted/killed `conform` run must never
// strand its hermetic scratch daemon, so the daemon self-terminates when this
// process (its parent) dies. Production daemons never set this, so their detached
// lifetime is unchanged. (The kill-the-parent exit path is covered by the daemon's
// watchdog goroutine + the exitWithParent env-gate unit test, not here, to avoid
// a flaky in-process fork test.)
func (s *Scratch) daemonEnv() []string {
	return append(s.env(), "MAD_EXIT_WITH_PARENT=1")
}

// initGovernedRepo creates the scratch governed git repo (the daemon's RepoRoot /
// cwd) with one seed commit, so the substrate can provision worktrees off HEAD.
func (s *Scratch) initGovernedRepo() error {
	if err := os.MkdirAll(s.RepoDir, 0o755); err != nil {
		return err
	}
	steps := [][]string{
		{"init", "-q", "-b", "main", s.RepoDir},
		{"-C", s.RepoDir, "config", "user.email", "harness@mad-substrate"},
		{"-C", s.RepoDir, "config", "user.name", "harness"},
	}
	for _, st := range steps {
		if out, err := s.runGit("", st...); err != nil {
			return fmt.Errorf("conformance: init repo %v: %w: %s", st, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(s.RepoDir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		return err
	}
	for _, st := range [][]string{{"add", "."}, {"commit", "-q", "-m", "seed"}} {
		if out, err := s.runGit(s.RepoDir, st...); err != nil {
			return fmt.Errorf("conformance: seed repo %v: %w: %s", st, err, out)
		}
	}
	return nil
}

// startDaemon launches the daemon (cwd = governed repo) and BLOCKS until the
// socket accepts a connection — readiness, never a sleep. A bounded timeout
// surfaces a stuck daemon (with its captured log) rather than hanging the gate.
func (s *Scratch) startDaemon() error {
	cmd := exec.Command(s.Binary, "daemon", "--socket", s.Socket)
	cmd.Dir = s.RepoDir
	cmd.Env = s.daemonEnv()
	s.daemonLog = &bytes.Buffer{}
	cmd.Stdout = s.daemonLog
	cmd.Stderr = s.daemonLog
	if err := cmd.Start(); err != nil {
		return err
	}
	s.daemon = cmd
	s.cleanup = append(s.cleanup, func() {
		if s.daemon != nil && s.daemon.Process != nil {
			_ = s.daemon.Process.Kill()
			_, _ = s.daemon.Process.Wait()
		}
	})

	// Poll the socket for acceptance (key off the EXPLICIT ready state).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("unix", s.Socket, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return nil
		}
		// If the daemon already exited, fail fast with its log.
		if s.daemon.ProcessState != nil && s.daemon.ProcessState.Exited() {
			return fmt.Errorf("conformance: daemon exited before ready: %s", s.daemonLog.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("conformance: daemon socket %s not ready within timeout: %s", s.Socket, s.daemonLog.String())
}

// DaemonLog returns the captured daemon stdout+stderr (diagnostics on failure).
func (s *Scratch) DaemonLog() string {
	if s.daemonLog == nil {
		return ""
	}
	return s.daemonLog.String()
}

// RestartDaemon kills the running daemon and starts a fresh one on the SAME socket,
// runtime dir, and ledger — a real daemon RESTART. The DURABLE ledger (leases +
// session tokens + the mediated trunk) persists, so a launcher that re-attaches via
// its capability token recovers its session. The session-reattach probe (P0 #4)
// uses this to prove a restart does not reclaim a still-live session's boundary.
func (s *Scratch) RestartDaemon() error {
	if s.daemon != nil && s.daemon.Process != nil {
		_ = s.daemon.Process.Kill()
		_, _ = s.daemon.Process.Wait()
		s.daemon = nil
	}
	return s.startDaemon()
}

// Close kills the daemon and removes every scratch dir (idempotent; LIFO).
func (s *Scratch) Close() {
	for i := len(s.cleanup) - 1; i >= 0; i-- {
		s.cleanup[i]()
	}
	s.cleanup = nil
}

// ----------------------------------------------------------------------------
// CLI runner — exec the built binary with the scratch env + --socket.
// ----------------------------------------------------------------------------

// CLIResult is the captured outcome of one CLI invocation.
type CLIResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error // non-nil only on a spawn failure (not a non-zero exit)
}

// OK reports whether the command exited 0.
func (r CLIResult) OK() bool { return r.ExitCode == 0 && r.Err == nil }

// Out is stdout+stderr joined (most CLI surfaces print to stdout; errors to
// stderr) — convenient for substring assertions.
func (r CLIResult) Out() string { return r.Stdout + r.Stderr }

// CLI runs `mad-substrate <args...> --socket <scratch>` with the hermetic env and
// captures stdout/stderr/exit. --socket is appended automatically (probe-writers
// pass only the subcommand + its args). A non-zero exit is NOT an error — it is a
// first-class observable (process exit codes are part of the public surface).
func (s *Scratch) CLI(args ...string) CLIResult {
	full := append(append([]string(nil), args...), "--socket", s.Socket)
	cmd := exec.Command(s.Binary, full...)
	cmd.Dir = s.RepoDir
	cmd.Env = s.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := CLIResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else {
			res.Err = err
			res.ExitCode = -1
		}
	}
	return res
}

// ----------------------------------------------------------------------------
// RPC dialer — dial the scratch socket with the WIRE client (frozen public
// protocol), exactly as any external client. Each Dial is a fresh CONNECTION, so
// a distinct connection mints a distinct daemon session identity (the substrate
// of the two-agent E2E and the rival-lease checks).
// ----------------------------------------------------------------------------

// Dial opens a new wire connection to the scratch daemon. Each connection is a
// distinct session (the daemon mints a connection-bound identity), so two Dials
// model two distinct agents.
func (s *Scratch) Dial() (*rpcclient.Client, error) {
	return rpcclient.Dial(s.Socket)
}

// WhoAmI returns the daemon-minted session id for a fresh connection (the public
// session.whoami). Used to learn an agent's identity for cross-session asserts.
func (s *Scratch) WhoAmI() (string, error) {
	c, err := s.Dial()
	if err != nil {
		return "", err
	}
	defer c.Close()
	return whoAmIOn(c)
}

// whoAmIOn returns the daemon-minted session id for an EXISTING connection (so the
// identity learned matches the connection that will hold a lease).
func whoAmIOn(c *rpcclient.Client) (string, error) {
	var out struct {
		Session string `json:"session"`
	}
	if err := c.Call("session.whoami", map[string]any{}, &out); err != nil {
		return "", err
	}
	return out.Session, nil
}

// leaseHolderFor returns the holder identity and count of live holders for a given
// base64 lease key, via the public lease.list (observable state). It is the
// black-box "who holds this lease right now" used to prove contention is on the
// single trunk lease (#14). Returns ("", 0) when the key is not held.
func (s *Scratch) leaseHolderFor(base64Key string) (holder string, count int) {
	c, err := s.Dial()
	if err != nil {
		return "", 0
	}
	defer c.Close()
	var out struct {
		Holders []struct {
			Key    string `json:"key"`
			Holder string `json:"holder"`
		} `json:"holders"`
	}
	if err := c.Call("lease.list", map[string]any{}, &out); err != nil {
		return "", 0
	}
	for _, h := range out.Holders {
		if h.Key == base64Key {
			holder = h.Holder
			count++
		}
	}
	return holder, count
}

// CallRaw invokes a public method on a fresh connection and returns the RAW result
// envelope bytes (canonicalized by the wire codec). It is the substrate of the
// joint-2b byte-identical-verdict probe: driving the daemon's dispatch/authz/
// routing path with identical inputs and asserting the response bytes are identical
// across repetitions (a probabilistic/LLM component would diverge).
func (s *Scratch) CallRaw(method string, params map[string]any) (string, error) {
	c, err := s.Dial()
	if err != nil {
		return "", err
	}
	defer c.Close()
	var raw json.RawMessage
	if err := c.Call(method, params, &raw); err != nil {
		return "", err
	}
	// Canonicalize: round-trip through a sorted-key re-marshal so map-iteration order
	// in the daemon's JSON encoder cannot create a spurious "difference".
	return canonicalJSON(raw)
}

// canonicalJSON re-marshals JSON with deterministic key ordering (encoding/json
// sorts map keys), so two semantically-identical envelopes compare byte-equal.
func canonicalJSON(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// IntegrationStatus returns an integration's reconciled state via the public
// integrate.status RPC (observable lifecycle state — received/validating/promoted/
// aborted). Used to poll for the EXPLICIT validating/aborted state, never a sleep.
func (s *Scratch) IntegrationStatus(id string) (state string, err error) {
	c, derr := s.Dial()
	if derr != nil {
		return "", derr
	}
	defer c.Close()
	var out struct {
		State string `json:"state"`
	}
	if cerr := c.Call("integrate.status", map[string]any{"id": id}, &out); cerr != nil {
		return "", cerr
	}
	return out.State, nil
}

// GateAccess is the structured result of singular.request (the gate's env-spec
// routing for a resource). Env is the FULL env map the gate produced — the leak
// detector enumerates every value, raw + decoded.
type GateAccess struct {
	Resource      string            `json:"resource"`
	Mode          string            `json:"mode"`
	Granted       bool              `json:"granted"`
	RealReachable bool              `json:"real_reachable"`
	Env           map[string]string `json:"env"`
	Reason        string            `json:"reason"`
}

// GateRequest drives the public singular.request RPC on a fresh connection and
// returns the structured Access (including the full env map). Used by the proxy /
// deny leak probes to enumerate EVERY env value (not just grep the CLI text).
func (s *Scratch) GateRequest(resource string) (GateAccess, error) {
	c, err := s.Dial()
	if err != nil {
		return GateAccess{}, err
	}
	defer c.Close()
	var a GateAccess
	if err := c.Call("singular.request", map[string]any{"resource": resource}, &a); err != nil {
		return GateAccess{}, err
	}
	return a, nil
}

// RouteLeaseKey returns the base64 lease key the classifier routes a (domain,
// name) to — the ONLY legitimate source of a lease key (never fabricate one). For
// the trunk it is classify.route{domain:"trunk"}. Returns ("", false) when the
// route yields no key (a non-convergent resource).
func (s *Scratch) RouteLeaseKey(domain, name string) (key string, ok bool, err error) {
	c, derr := s.Dial()
	if derr != nil {
		return "", false, derr
	}
	defer c.Close()
	var out struct {
		Kind     string  `json:"kind"`
		LeaseKey *string `json:"lease_key"`
	}
	if cerr := c.Call("classify.route", map[string]any{"domain": domain, "name": name}, &out); cerr != nil {
		return "", false, cerr
	}
	if out.LeaseKey == nil {
		return "", false, nil
	}
	return *out.LeaseKey, true, nil
}

// ----------------------------------------------------------------------------
// Git helper — drive real `git` with the hermetic env (the uncooperative agent
// uses RAW git; cooperation is never assumed).
// ----------------------------------------------------------------------------

// Git runs `git -C <dir> <args...>` with the hermetic env and returns combined
// output + error. dir "" runs without -C.
func (s *Scratch) Git(dir string, args ...string) (string, error) {
	return s.runGit(dir, args...)
}

func (s *Scratch) runGit(dir string, args ...string) (string, error) {
	var full []string
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = s.env()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// GitOK runs Git and returns true iff it exited 0 (for boolean structural probes
// like "the trunk-protect hook refused this push").
func (s *Scratch) GitOK(dir string, args ...string) bool {
	_, err := s.runGit(dir, args...)
	return err == nil
}

// RefExists reports whether a ref resolves in a git dir (e.g. the trunk ref in
// the mediated bare repo). Used to assert a rejected push left no ref behind.
func (s *Scratch) RefExists(gitDir, ref string) bool {
	return s.GitOK(gitDir, "rev-parse", "--verify", "--quiet", ref)
}

// ----------------------------------------------------------------------------
// Fixture builder — a scratch agent working repo whose origin is the mediated
// bare trunk, plus the established-trunk-base helper, so a probe drives a REAL
// black-box integration (push nm/*, submit, promote).
// ----------------------------------------------------------------------------

// Agent is one agent's working repo: origin -> the mediated bare trunk repo. It
// pushes nm/* feature branches (the only refs the hook accepts) which the
// integrator then promotes. Two Agents over disjoint dirs model two agents.
type Agent struct {
	s    *Scratch
	Dir  string // the agent's working repo
	Name string
}

// NewAgent creates an agent working repo with origin pointed at the mediated bare
// trunk repo (<Root>/trunk.git, created by the daemon on boot). The repo starts
// empty; use Commit + PushBranch to author work.
func (s *Scratch) NewAgent(name string) (*Agent, error) {
	dir := filepath.Join(s.Root, "agent-"+name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	a := &Agent{s: s, Dir: dir, Name: name}
	steps := [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "config", "user.email", name + "@mad-substrate"},
		{"-C", dir, "config", "user.name", name},
		{"-C", dir, "remote", "add", "origin", s.BareDir},
	}
	for _, st := range steps {
		if out, err := s.runGit("", st...); err != nil {
			return nil, fmt.Errorf("conformance: new agent %v: %w: %s", st, err, out)
		}
	}
	return a, nil
}

// Commit writes files into the agent's working repo and commits them on the
// current branch, returning the new HEAD oid.
func (a *Agent) Commit(message string, files map[string]string) (string, error) {
	for rel, content := range files {
		p := filepath.Join(a.Dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	for _, st := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", message}} {
		if out, err := a.s.runGit(a.Dir, st...); err != nil {
			return "", fmt.Errorf("conformance: agent commit %v: %w: %s", st, err, out)
		}
	}
	return a.Head()
}

// Head returns the agent's current HEAD commit oid.
func (a *Agent) Head() (string, error) {
	out, err := a.s.runGit(a.Dir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("conformance: agent head: %w: %s", err, out)
	}
	return strings.TrimSpace(out), nil
}

// Checkout makes a working branch off a ref (e.g. origin/trunk), fetching origin
// first so origin/trunk reflects the latest promoted tip.
func (a *Agent) Checkout(workBranch, fromRef string) error {
	if out, err := a.s.runGit(a.Dir, "fetch", "-q", "origin"); err != nil {
		return fmt.Errorf("conformance: agent fetch: %w: %s", err, out)
	}
	if out, err := a.s.runGit(a.Dir, "checkout", "-q", "-B", workBranch, fromRef); err != nil {
		return fmt.Errorf("conformance: agent checkout %s from %s: %w: %s", workBranch, fromRef, err, out)
	}
	return nil
}

// PushBranch pushes the agent's HEAD to the mediated bare repo as
// refs/heads/nm/<name> (the ref family the trunk-protect hook accepts) and
// returns the bare-side ref the integrator submits.
func (a *Agent) PushBranch(name string) (string, error) {
	ref := "refs/heads/nm/" + name
	if out, err := a.s.runGit(a.Dir, "push", "-q", "origin", "HEAD:"+ref); err != nil {
		return "", fmt.Errorf("conformance: agent push %s: %w: %s", ref, err, out)
	}
	return ref, nil
}

// EstablishTrunkBase authors a base commit on agent a, pushes it as nm/base, and
// promotes it via the public CLI so the mediated trunk is BORN (unborn-trunk
// create-only path). It returns the bare-side trunk tip after promotion. Probes
// that need an existing trunk (a clean feature promote, a conflicting branch)
// call this first.
func (s *Scratch) EstablishTrunkBase(a *Agent, baseFiles map[string]string) (trunkTip string, err error) {
	if _, err := a.Commit("base", baseFiles); err != nil {
		return "", err
	}
	ref, err := a.PushBranch("base")
	if err != nil {
		return "", err
	}
	id, err := s.SubmitAndPromote(ref)
	if err != nil {
		return "", err
	}
	_ = id
	return s.TrunkTip()
}

// SubmitAndPromote runs `trunk submit <ref>` then `trunk promote <id>` over the
// PUBLIC CLI, returning the integration id. It errors if either CLI step fails to
// reach a promoted outcome (the caller asserts the trunk effect separately).
func (s *Scratch) SubmitAndPromote(branchRef string) (id string, err error) {
	id, err = s.Submit(branchRef)
	if err != nil {
		return "", err
	}
	res := s.CLI("trunk", "promote", id)
	if !res.OK() {
		return id, fmt.Errorf("conformance: promote %s failed (exit %d): %s", id, res.ExitCode, res.Out())
	}
	if !strings.Contains(res.Out(), "promoted") {
		return id, fmt.Errorf("conformance: promote %s did not report promoted: %s", id, res.Out())
	}
	return id, nil
}

// SubmitOn submits a branch over an EXISTING wire connection, so the integration's
// holder is THAT connection's session (not an ephemeral CLI process). This lets a
// probe make the holder genuinely LIVE (the same connection then holds a lease) or
// genuinely dead (it holds none) — the substrate of the mid-integration abort check
// and its live-holder control. Returns the integration id.
func (s *Scratch) SubmitOn(c *rpcclient.Client, branchRef string) (id string, err error) {
	var out struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if cerr := c.Call("integrate.submit", map[string]any{"branch": branchRef}, &out); cerr != nil {
		return "", cerr
	}
	if out.ID == "" {
		return "", fmt.Errorf("conformance: integrate.submit returned no id")
	}
	return out.ID, nil
}

// Submit runs `trunk submit <ref>` and parses the integration id from the public
// CLI output ("submitted <id> (<state>, base <b>)").
func (s *Scratch) Submit(branchRef string) (id string, err error) {
	res := s.CLI("trunk", "submit", branchRef)
	if !res.OK() {
		return "", fmt.Errorf("conformance: submit %s failed (exit %d): %s", branchRef, res.ExitCode, res.Out())
	}
	id = parseSubmitID(res.Stdout)
	if id == "" {
		return "", fmt.Errorf("conformance: could not parse integration id from %q", res.Stdout)
	}
	return id, nil
}

// PromoteOn drives integrate.promote over an EXISTING connection and returns the
// outcome's state + retryable flag. A promote that reaches `validating` but is
// blocked at the trunk-lease step (a rival holds it) returns state=validating,
// retryable=true — the in-flight state the mid-integration abort check targets.
func (s *Scratch) PromoteOn(c *rpcclient.Client, id string) (state string, retryable bool, err error) {
	var out struct {
		State     string `json:"state"`
		Retryable bool   `json:"retryable"`
		Promoted  bool   `json:"promoted"`
	}
	if cerr := c.Call("integrate.promote", map[string]any{"id": id}, &out); cerr != nil {
		return "", false, cerr
	}
	return out.State, out.Retryable, nil
}

// TrunkTip returns the mediated trunk's current commit oid via raw git on the
// bare repo (observable state — the authoritative ref the integrator advanced).
// Returns ("", nil) when trunk is unborn.
func (s *Scratch) TrunkTip() (string, error) {
	out, err := s.runGit(s.BareDir, "rev-parse", "--verify", "--quiet", "refs/heads/trunk")
	if err != nil {
		if strings.TrimSpace(out) == "" {
			return "", nil // unborn
		}
		return "", fmt.Errorf("conformance: trunk tip: %w: %s", err, out)
	}
	return strings.TrimSpace(out), nil
}

// parseSubmitID extracts <id> from "submitted <id> (<state>, base <b>)".
func parseSubmitID(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		const prefix = "submitted "
		if strings.HasPrefix(line, prefix) {
			rest := strings.TrimPrefix(line, prefix)
			if i := strings.IndexByte(rest, ' '); i > 0 {
				return rest[:i]
			}
			return rest
		}
	}
	return ""
}

// ----------------------------------------------------------------------------
// SpawnInfo — parse `mad-substrate spawn` stdout into the boundary fields a forkable-
// isolation probe asserts over (cwd, branch, ports, session).
// ----------------------------------------------------------------------------

// SpawnInfo is the parsed output of `mad-substrate spawn`.
type SpawnInfo struct {
	Session string
	Cwd     string
	Branch  string
	Ports   []int
}

// Spawn provisions an isolated forkable boundary over the PUBLIC CLI and parses
// its reported cwd/branch/ports/session. Each Spawn is a fresh CLI process →
// distinct daemon session → distinct boundary (the substrate of conjunct (a)).
func (s *Scratch) Spawn() (SpawnInfo, CLIResult, error) {
	res := s.CLI("spawn")
	if !res.OK() {
		return SpawnInfo{}, res, fmt.Errorf("conformance: spawn failed (exit %d): %s", res.ExitCode, res.Out())
	}
	info, err := parseSpawn(res.Stdout)
	return info, res, err
}

// Boundary is the FULL provisioned forkable boundary (the public substrate.provision
// Wire) — richer than SpawnInfo: it carries the routed env map + per-agent
// local-state dirs, which the escape-resistance probe needs to assert containment
// of a path-traversal resource ref. A fresh connection per call mints a distinct
// session → a distinct boundary.
type Boundary struct {
	Session string `json:"session"`
	Grain   string `json:"grain"`
	Cwd     string `json:"cwd"`
	Branch  string `json:"branch"`
	// HostWorktree and ContainerID are the additive container-grain wire fields: at
	// the container grain Cwd is the IN-CONTAINER mount (/work), HostWorktree is the
	// real host dir bind-mounted there, and ContainerID is the confined container the
	// agent execs into. Both are "" at the worktree grain (the wire omits them), so the
	// worktree-grain probes that read Cwd/Branch/Ports are unaffected.
	HostWorktree string            `json:"host_worktree"`
	ContainerID  string            `json:"container_id"`
	Ports        []int             `json:"ports"`
	Env          map[string]string `json:"env"`
	StateDirs    map[string]string `json:"state_dirs"`
}

// StateRoot returns the agent's local-state root: the deepest common ancestor of
// the per-role state dirs (scratch/cache/state all live under the agent root).
// Returns "" when there are no state dirs.
func (b Boundary) StateRoot() string {
	var common string
	for _, p := range b.StateDirs {
		if p == "" {
			continue
		}
		parent := filepath.Dir(p)
		if common == "" {
			common = parent
			continue
		}
		common = commonAncestor(common, parent)
	}
	return common
}

// commonAncestor returns the deepest directory that contains both a and b.
func commonAncestor(a, b string) string {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	for a != b {
		if len(a) > len(b) {
			a = filepath.Dir(a)
		} else {
			b = filepath.Dir(b)
		}
		if a == "." || a == string(filepath.Separator) || b == "." || b == string(filepath.Separator) {
			if a == b {
				return a
			}
			break
		}
	}
	if a == b {
		return a
	}
	return string(filepath.Separator)
}

// pathWithin reports whether p resolves at or inside dir (lexical containment after
// Clean/Abs). Empty dir → false.
func pathWithin(p, dir string) bool {
	if dir == "" {
		return false
	}
	return nested(dir, p)
}

// escapesTo reports whether p resolves at or inside a target prefix dir (e.g.
// "/etc") — an escape the worktree boundary must never hand out.
func escapesTo(p, target string) bool {
	return nested(target, p)
}

// provisionResource is one resource the substrate classifies + (if forkable)
// routes into the boundary's env (used to drive a path-traversal ref).
type provisionResource struct {
	Name   string `json:"name"`
	Domain string `json:"domain"`
	Ref    string `json:"ref"`
}

// Provision drives the public substrate.provision RPC on a FRESH connection
// (distinct session → distinct boundary) and returns the full Wire boundary,
// optionally declaring resources (each classified + routed by the substrate). It
// is the surface the worktree-grain escape-resistance probe drives directly: it
// can hand the substrate a path-traversal resource ref and assert the env path the
// substrate hands back is CONTAINED, never an escaping absolute/sibling path.
func (s *Scratch) Provision(resources ...provisionResource) (Boundary, error) {
	c, err := s.Dial()
	if err != nil {
		return Boundary{}, err
	}
	defer c.Close()
	params := map[string]any{}
	if len(resources) > 0 {
		params["resources"] = resources
	}
	var b Boundary
	if err := c.Call("substrate.provision", params, &b); err != nil {
		return Boundary{}, err
	}
	return b, nil
}

// parseSpawn parses the spawn stdout block:
//
//	spawned <grain> boundary for session <sess>
//	  cwd:    <path>
//	  branch: <nm/...>
//	  ports:  [p p p p]
func parseSpawn(stdout string) (SpawnInfo, error) {
	var info SpawnInfo
	for _, raw := range strings.Split(stdout, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "spawned "):
			if i := strings.Index(line, "for session "); i >= 0 {
				info.Session = strings.TrimSpace(line[i+len("for session "):])
			}
		case strings.HasPrefix(line, "cwd:"):
			info.Cwd = strings.TrimSpace(strings.TrimPrefix(line, "cwd:"))
		case strings.HasPrefix(line, "branch:"):
			info.Branch = strings.TrimSpace(strings.TrimPrefix(line, "branch:"))
		case strings.HasPrefix(line, "ports:"):
			info.Ports = parsePorts(strings.TrimSpace(strings.TrimPrefix(line, "ports:")))
		}
	}
	if info.Session == "" || info.Cwd == "" {
		return info, fmt.Errorf("conformance: could not parse spawn output: %q", stdout)
	}
	return info, nil
}

// parsePorts parses "[55662 55663 55664 55665]" into a slice of ints.
func parsePorts(s string) []int {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	var out []int
	for _, f := range strings.Fields(s) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}
