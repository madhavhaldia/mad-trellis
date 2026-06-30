package launcher

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/madhavhaldia/mad-substrate/internal/conductor"
	"github.com/madhavhaldia/mad-substrate/internal/coopwiring"
	"github.com/madhavhaldia/mad-substrate/internal/manifest"
	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
	"github.com/madhavhaldia/mad-substrate/internal/runtimecfg"
	"github.com/madhavhaldia/mad-substrate/internal/substrate"
)

// coopSocketPath is where the in-container relay listens and where the cooperative
// layer (mad-substrate mcp / hook) connects (its MAD_SOCKET). It lives on the
// container's writable tmpfs /tmp — NOT under /work, so it never appears in the
// agent's git clone — and is short enough to satisfy the unix-socket path-length
// limit.
const coopSocketPath = "/tmp/mad-substrate-coop.sock"

// coopRelayEnv is the OVERRIDE knob: when set to a host path of the static linux
// relay binary it WINS over the embedded relay (resolveRelayHostPath precedence #1).
// The cooperative plane is now ON BY DEFAULT for the container grain — the launcher
// auto-resolves the relay from the binary's own embedded payload (internal/coopembed,
// a -tags coopembed build), so this env is only needed to point at a DIFFERENT relay
// (a custom build, or an arch the shipped binary did not embed). Set to a path that
// does not exist / cannot start → fail-soft (the agent runs confined without the
// plane), never a block.
const coopRelayEnv = "MAD_CONTAINER_RELAY"

// sessionLeaseTTL is the session-liveness lease's TTL. While the launcher renews
// it (at ~TTL/2) the session is alive; when the launcher stops renewing (clean
// exit or crash) it lapses within one TTL and the session is declared dead by
// liveness (project 8). A short-but-tolerant 30s balances fast death detection
// against renew chatter and a transient renew blip.
const sessionLeaseTTL = 30 * time.Second

// cleanExitTimeout bounds the ENTIRE clean-exit teardown — the renew-goroutine
// join, lease release, AND boundary teardown — so a wedged daemon connection can
// never demote the NORMAL exit path into an indefinite hang. (rpcclient now bounds
// each call by DefaultReadTimeout, but that is ~120s; this 5s budget keeps exit
// snappy and covers a renew Call caught mid-flight.) If teardown does not complete
// in time we abandon it and let the dropped connection + lease TTL / liveness
// (project 8) reclaim — the launcher always makes progress on exit.
const cleanExitTimeout = 5 * time.Second

// BlockedExitCode is the exit code the launcher returns when it FAILS CLOSED —
// it establishes no governed session and therefore did NOT run the agent. It is
// distinct from any code the agent itself could return (126 = "command found
// but could not be executed", POSIX), so a caller/script can tell "blocked,
// never ran" apart from the agent's own exit.
const BlockedExitCode = 126

// SpawnFunc runs the opaque agent on a PTY into the boundary `target` describes
// (host worktree grain, or INSIDE a confined container at the container grain),
// with env applied, forwarding signals/resize and propagating the child's exact
// exit code (Inv 13 ambient interaction). Injected so the fail-closed tests can
// assert whether the agent was EVER reached without exec'ing a real process.
type SpawnFunc func(target ExecTarget, env map[string]string, agent string, args []string) (int, error)

// Config parameterizes one governed launch.
//
// NEGATIVE OBLIGATION — no-goals/no-dispatch (Inv 13, [GATED]): there is
// deliberately NO goal / task / prompt / objective field on this struct. The
// launcher execs an OPAQUE agent command and forwards Args VERBATIM; it never
// accepts, injects, or dispatches a unit of work. This keeps the Layer-2
// task-intake seam EMPTY (docs/0003 12-intake). intake_test.go pins it with a
// positive control (adding a goal-accepting flag turns the audit RED).
type Config struct {
	Socket    string                  // daemon socket ("" → default path, resolved by the caller)
	Agent     string                  // the agent command (opaque); RAW — resolved by Run on the AUTHORITATIVE grain
	Args      []string                // the agent's OWN args, forwarded verbatim — never a goal
	Ports     int                     // ports to request (0 → substrate default)
	Resources []substrate.ResourceReq // declared resources for the substrate to classify+route

	// ResolveAgent resolves the raw Agent to a real executable on the HOST,
	// fail-closed and with the mad-substrate shim EXCLUDED (so a bare name can never
	// resolve back to our own shim). Run calls it ONLY for the host/worktree grain;
	// at the container grain the Agent is passed THROUGH (resolved in-container by
	// `container exec`). nil → identity (used by tests). Resolving on the grain the
	// daemon AUTHORITATIVELY reports (spec.Grain) keeps resolution and exec from
	// diverging — the C34 / shim-loop fix.
	ResolveAgent func(agentArg string) (string, error)
	// RequestedContainerGrain is true when the CALLER asked for the container grain
	// (--grain container / MAD_GRAIN=container). If the daemon cannot honor it
	// (its authoritative spec.Grain is host/worktree — e.g. a pre-existing worktree
	// daemon), Run fails CLOSED rather than silently running on the host: that makes
	// "a grain mismatch yields a clean non-run" actually true.
	RequestedContainerGrain bool

	// Injected seams (nil → production implementations). Tests override these to
	// drive the fail-closed and clean-exit paths deterministically.
	Dial  Dialer
	Spawn SpawnFunc
	Logf  func(format string, args ...any) // diagnostics sink (nil → discard)

	// sessionLeaseTTL overrides the session-liveness lease TTL (0 → the production
	// default). It is unexported so it is NOT part of the public launch surface;
	// only same-package tests set it (a short TTL keeps the renew-cadence test fast
	// without a sleep-for-luck).
	sessionLeaseTTL time.Duration
}

// Run executes one governed agent session, fail-closed end to end, and returns
// the exit code to propagate. The control flow IS the Inv-4 guarantee:
//
//  1. Establish a governed session (held connection + daemon identity). FAIL →
//     BLOCK. There is no branch from here to Spawn.
//  2. Provision the isolation boundary on the held connection. FAIL → BLOCK.
//     (A failed provision is fully rolled back by the substrate — no orphan.)
//  3. ONLY now exec the agent into cwd+env on a PTY.
//  4. Clean-exit teardown (deferred, runs on EVERY return once provisioned):
//     release this session's leases + tear the boundary down. Idempotent, zero
//     orphans. The launcher owns the NORMAL exit path; a launcher CRASH (SIGKILL)
//     is liveness's job (project 8) via the dropped connection + lease TTL.
func Run(cfg Config) (exitCode int, err error) {
	dial := cfg.Dial
	if dial == nil {
		dial = func(socket string) (Conn, error) { return rpcclient.Dial(socket) }
	}
	spawn := cfg.Spawn
	if spawn == nil {
		spawn = RunPTY
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// (1) Establish the governed session. ANY failure here is fail-closed: the
	// agent is NOT run. "If the daemon is down, just run the agent" is the silent
	// Inv-4 hole this guard exists to forbid.
	sess, err := Open(dial, cfg.Socket)
	if err != nil {
		return BlockedExitCode, fmt.Errorf("BLOCKED: cannot establish a governed session (%w); refusing to launch %q ungoverned", err, cfg.Agent)
	}
	defer sess.Close()

	// (2) Provision the boundary on the held connection. Fail-closed.
	spec, err := sess.Provision(cfg.Ports, cfg.Resources)
	if err != nil {
		return BlockedExitCode, fmt.Errorf("BLOCKED: cannot provision an isolation boundary (%w); refusing to launch %q ungoverned", err, cfg.Agent)
	}

	// The boundary now exists → clean-exit teardown is owed on EVERY return path
	// below (normal exit, spawn error, a fail-closed mint/acquire after this point,
	// panic-free early return). Registered IMMEDIATELY after provision (before the
	// session-liveness setup that follows) so a boundary is NEVER leaked: even if
	// the mint/acquire fails-closed below, this defer reclaims the boundary. The
	// renew goroutine (started later) is stopped+joined INSIDE the bounded region
	// FIRST (before the release) so no renew can race the ReleaseOwnLeases that frees
	// the session-liveness lease (and any adapter leases under the shared id — C11).
	// stopRenew is closed only if the goroutine was actually started (renewStarted).
	// Idempotent and best-effort: teardown failures are diagnosed but never mask the
	// agent's exit code, and — the WHOLE sequence bounded by cleanExitTimeout — never
	// leave the launcher hung on a wedged connection (even a renew caught mid-Call).
	stopRenew := make(chan struct{})
	renewDone := make(chan struct{})
	renewStarted := false
	defer func() {
		done := make(chan struct{})
		go func() {
			// Stop + join the renew goroutine FIRST (before releasing the
			// session-liveness lease) so no in-flight renew races ReleaseOwnLeases.
			// This runs INSIDE the cleanExitTimeout budget, so even a renew Call caught
			// mid-flight against a wedged daemon cannot stall exit past the budget — the
			// select abandons and lease TTL / liveness reclaims (the process then exits,
			// ending the renew goroutine).
			if renewStarted {
				close(stopRenew)
				<-renewDone
			}
			if rerr := sess.ReleaseOwnLeases(); rerr != nil {
				logf("clean-exit: lease release: %v", rerr)
			}
			if terr := sess.Teardown(); terr != nil {
				logf("clean-exit: boundary teardown: %v", terr)
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(cleanExitTimeout):
			logf("clean-exit: teardown exceeded %s; abandoning (lease TTL / liveness will reclaim)", cleanExitTimeout)
		}
	}()

	// (2a) Resolve the agent on the grain the daemon AUTHORITATIVELY provisioned
	// (spec.Grain), so resolution and the exec path never diverge:
	//   - CONTAINER: pass the Agent through; `container exec` resolves it in the guest
	//     (the in-container PATH / an in-container path). Host-stat'ing it would BLOCK
	//     an agent that exists only in the image (chafe C34).
	//   - HOST/WORKTREE: resolve on the host with the shim EXCLUDED (ResolveAgent), so
	//     a bare name can NEVER resolve back to our own shim and re-exec into a loop.
	// A CONTAINER request the daemon cannot honor (it is a host/worktree daemon) fails
	// CLOSED here — never a silent host run — so a grain mismatch is a clean non-run.
	// The boundary-teardown defer above already covers a fail-closed return.
	agent := cfg.Agent
	if spec.Grain != containerGrainName {
		if cfg.RequestedContainerGrain {
			return BlockedExitCode, fmt.Errorf("BLOCKED: --grain container was requested but the running daemon provisions %q boundaries; stop it (`mad-substrate daemon stop`) and re-launch so a container-grain daemon starts, rather than run %q on the host", grainLabel(spec.Grain), cfg.Agent)
		}
		if cfg.ResolveAgent != nil {
			resolved, rerr := cfg.ResolveAgent(cfg.Agent)
			if rerr != nil {
				return BlockedExitCode, fmt.Errorf("BLOCKED: cannot resolve agent %q (%w); refusing to launch ungoverned", cfg.Agent, rerr)
			}
			agent = resolved
		}
	}

	// (2b) Establish the session-liveness identity (T2): mint an unforgeable token
	// bound to the held connection, then acquire the session-liveness lease under
	// the daemon-returned key. The lease's TTL is the ONE TRUE session-death signal
	// (liveness, project 8) AND the gate session.attach checks. Fail-closed: an
	// agent that cannot have a live, shareable governed identity must not run — the
	// adapter (a separate process) relies on attaching to it. The boundary teardown
	// defer above already covers a fail-closed return here.
	token, livenessKey, err := sess.MintToken()
	if err != nil {
		return BlockedExitCode, fmt.Errorf("BLOCKED: cannot mint a session token (%w); refusing to launch %q ungoverned", err, cfg.Agent)
	}
	ttl := sessionLeaseTTL
	if cfg.sessionLeaseTTL > 0 {
		ttl = cfg.sessionLeaseTTL
	}
	if err := sess.AcquireSessionLease(livenessKey, ttl); err != nil {
		return BlockedExitCode, fmt.Errorf("BLOCKED: cannot acquire the session-liveness lease (%w); refusing to launch %q ungoverned", err, cfg.Agent)
	}

	// Export the token into the agent env (alongside MAD_SESSION). The
	// cooperative layer (mad-substrate mcp / hook) reads MAD_SESSION_TOKEN and
	// session.attach'es it to act under the SHARED session identity (Inv 4: the
	// token is the only identity path; the cooperative client can never name the
	// session id directly). spec.Env is a COPY (EnvSpec.Env hands back a copy), so
	// mutating it here is safe.
	env := spec.Env
	if env == nil {
		env = map[string]string{}
	}
	env["MAD_SESSION_TOKEN"] = token

	// The renew goroutine keeps the session-liveness lease alive at ~TTL/2 until
	// clean exit. It is bounded (stops on stopRenew, joined by the teardown defer)
	// and RECOVERS across a DAEMON RESTART (P0 #4): on a renew failure it re-attaches
	// via the capability token (which resolves against the daemon's DURABLE token
	// store) and renews as the restored identity, so the lease never lapses and
	// liveness never reclaims a still-running session's boundary. See keepalive.go.
	renewStarted = true
	go func() {
		defer close(renewDone)
		runSessionKeepalive(sess, livenessKey, token, ttl, stopRenew, logf)
	}()

	logf("governed session %s: cwd=%s ports=%v", sess.ID(), spec.Cwd, spec.Ports)

	// (2c) COOPERATIVE PLANE into the container (#2, ON BY DEFAULT, FAIL-SOFT). When
	// the boundary is a container, run the exec-stdio relay so the in-container
	// cooperative client can reach the daemon (it has no host socket path, and the
	// confined default is --network none). The relay is AUTO-RESOLVED — from the
	// MAD_CONTAINER_RELAY override if set, else from the binary's own embedded
	// payload (internal/coopembed) — so no env is needed; an untagged build with no
	// override resolves to "" and simply runs confined. The agent's MAD_SOCKET
	// is pointed at the in-container relay socket; the cooperative client's own
	// token-authed session.attach (MAD_SESSION_TOKEN, already in env) rebinds
	// each forwarded connection to THIS session. FAIL-SOFT: a relay that cannot
	// resolve or will not start NEVER blocks the agent — the hard floor already
	// confines it; it simply runs without the coordination plane. The stop defer runs
	// BEFORE the boundary-teardown defer (LIFO), so the tunnel is closed before
	// `container rm`.
	// coopPlaneUp records whether the in-container cooperative plane (2c) ACTUALLY came
	// up, so the in-container MCP wiring (2e) fires ONLY when the agent can reach the
	// daemon through the relay (MAD_SOCKET → the relay socket). Wiring an MCP
	// command that dials a socket the relay never bound would be pointless.
	coopPlaneUp := false
	if spec.Grain == containerGrainName && spec.ContainerID != "" {
		relayPath, cleanup, rerr := resolveRelayHostPath(runtime.GOARCH)
		switch {
		case rerr != nil:
			logf("coop: cooperative plane relay unavailable (%v); running the agent confined WITHOUT it", rerr)
		case relayPath == "":
			logf("coop: no cooperative-plane relay (no %s override and none embedded for %s); running the agent confined WITHOUT it", coopRelayEnv, runtime.GOARCH)
		default:
			daemonSock := cfg.Socket
			if daemonSock == "" {
				daemonSock = runtimecfg.SocketPath("")
			}
			stop, cerr := startCoop(coopConfig{
				containerBin:    containerBin,
				containerID:     spec.ContainerID,
				relayHostPath:   relayPath,
				scratchDir:      env["MAD_SCRATCH"],
				inContainerSock: coopSocketPath,
				daemonSocket:    daemonSock,
				logf:            logf,
			})
			// startCoop has COPIED the relay into the container's scratch dir (or
			// failed); the resolved host source — a temp file when it came from the
			// embedded payload — is no longer needed either way.
			if cleanup != nil {
				cleanup()
			}
			if cerr != nil {
				logf("coop: cooperative plane unavailable (%v); running the agent confined WITHOUT it", cerr)
			} else {
				env["MAD_SOCKET"] = coopSocketPath
				coopPlaneUp = true
				defer stop()
				logf("coop: cooperative plane up; in-container MAD_SOCKET=%s", coopSocketPath)
			}
		}
		// Operator-visible coop STATUS (VISIBILITY ONLY — fail-soft, no behavior change).
		// The detailed reasons above go through logf, which the production launcher routes
		// to os.Stderr ONLY under MAD_DEBUG (nil/discard otherwise) — so by default
		// the operator never learns whether the cooperative plane came up. This ONE concise
		// line goes UNCONDITIONALLY to os.Stderr — the SAME stream and "mad-substrate:" prefix
		// the post-exec conductor status lines use — and is emitted PRE-exec, before RunPTY
		// puts the terminal in raw mode and bridges stdout to the child PTY, so it reliably
		// survives to the operator at launch.
		if coopPlaneUp {
			fmt.Fprintln(os.Stderr, "mad-substrate: cooperative plane up (in-container coordination enabled)")
		} else {
			fmt.Fprintln(os.Stderr, "mad-substrate: cooperative plane unavailable — agent runs confined without it")
		}
	}

	// (2d) COOPERATIVE-BY-DEFAULT WIRING (worktree grain). Generate the agent's MCP
	// server config (and, for Claude, the SessionStart standing-guidance hook) INTO
	// the disposable boundary worktree, pointing at THIS binary's own `mcp`/`hook`
	// subcommands (the cooperative layer
	// is native Go, not a separate adapter), so a launched claude/codex comes up
	// cooperative with zero manual setup. The wiring also git-EXCLUDES the files it
	// writes so the agent's commits — which the integrator merges to trunk — can
	// never carry cooperative config into the validated trunk. FAIL-SOFT (Inv 13):
	// any wiring failure is logged and the agent still launches (governed, just not
	// cooperative). The CONTAINER grain is wired separately in (2e) — this binary is
	// darwin and absent from the guest image, so that path stages the embedded LINUX
	// binary and points the MCP config at THAT instead.
	agentArgs := cfg.Args
	if spec.Grain != containerGrainName {
		if host := coopHost(cfg.Agent); host != "" {
			wtDir := spec.HostWorktree
			if wtDir == "" {
				wtDir = spec.Cwd
			}
			if bin, berr := coopwiring.BinaryPath(); berr != nil {
				logf("coop: cannot resolve the mad-substrate binary (%v); launching %q without cooperative wiring", berr, cfg.Agent)
			} else if res, werr := coopwiring.Wire(host, wtDir, bin); werr != nil {
				logf("coop: cooperative wiring failed (%v); launching %q governed but uncooperative", werr, cfg.Agent)
			} else {
				// Codex carries its MCP server via -c overrides + a hook-trust bypass
				// that must PRECEDE the user's own args; Claude's ExtraArgs is empty.
				agentArgs = append(append([]string{}, res.ExtraArgs...), cfg.Args...)
				if len(res.Wrote) > 0 || len(res.ExtraArgs) > 0 {
					logf("coop: wired %s cooperative layer (mcp + hooks)", host)
				}
			}
		}
	}

	// (2e) IN-CONTAINER MCP WIRING (container grain, FAIL-SOFT). When the cooperative
	// plane came up (2c), a real claude/codex INSIDE the container can reach the daemon
	// through the relay (MAD_SOCKET → the relay socket; MAD_SESSION_TOKEN is
	// already in the exec env). But THIS binary is darwin and absent from the guest, so
	// we STAGE the embedded static LINUX mad-substrate binary into the container's writable
	// scratch (the same stage mechanism as the relay) and run coopwiring.Wire against
	// the IN-CONTAINER CLONE (spec.HostWorktree, bind-mounted at /work), pointing the
	// MCP server `command` at the STAGED in-container mad-substrate path with arg "mcp". The
	// in-container agent then runs `<staged> mcp`, which dials MAD_SOCKET and
	// session.attach'es with MAD_SESSION_TOKEN to act under THIS session (Inv 4).
	// coopwiring git-EXCLUDES every file it writes — the clone is a real git repo whose
	// commits the integrator merges to trunk — so the cooperative config can never reach
	// the validated trunk. FAIL-SOFT (Inv 13): any staging/wiring failure is logged and
	// the agent still launches (governed + confined, just not cooperatively wired).
	if spec.Grain == containerGrainName && coopPlaneUp {
		if host := coopHost(cfg.Agent); host != "" {
			if res, werr := wireContainerMCP(host, spec.HostWorktree, env["MAD_SCRATCH"], runtime.GOARCH, logf); werr != nil {
				logf("coop: in-container MCP wiring failed (%v); launching %q governed+confined but uncooperative", werr, cfg.Agent)
			} else {
				// agentArgs is still cfg.Args here (the worktree branch above is skipped
				// for the container grain); prepend Codex's `-c` overrides (empty for Claude).
				agentArgs = append(append([]string{}, res.ExtraArgs...), agentArgs...)
				if len(res.Wrote) > 0 || len(res.ExtraArgs) > 0 {
					logf("coop: wired %s in-container cooperative layer (mcp via staged mad-substrate)", host)
				}
			}
		}
	}

	// Capture the convergence target from the launch cwd (the worktree the user
	// launched from) BEFORE the agent runs, and resolve the conductor policy from
	// the repo manifest. All best-effort: any failure disables auto-convergence
	// (fall back to manual `mad-substrate integrate`), it never blocks the launch.
	launchCwd, _ := os.Getwd()
	var targetBranch, repoRoot, gateCmd string
	conductorEnabled := false
	if launchCwd != "" {
		if b, e := gitLine(launchCwd, "rev-parse", "--abbrev-ref", "HEAD"); e == nil {
			targetBranch = b
		}
		if r, e := gitLine(launchCwd, "rev-parse", "--show-toplevel"); e == nil {
			repoRoot = r
		}
		if repoRoot != "" {
			if m, e := manifest.Load(repoRoot); e == nil {
				conductorEnabled = m.ConductorEnabled
				gateCmd = m.ConductorGate
			}
		}
	}
	// MAD_CONDUCTOR=off|0|false is the legacy opt-out (kept working): an
	// external orchestrator (Bridge Swarm / a Kanban layer) takes manual control. It
	// composes with the R10 MAD_CONVERGE_MODE knob below ("if EITHER says off,
	// it's off") and is resolved through convergeDecision so the off path still
	// surfaces the staged + manual-integrate hint instead of vanishing silently.
	conductorOff := false
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("MAD_CONDUCTOR"))); v == "off" || v == "0" || v == "false" {
		conductorOff = true
	}
	// We can only converge onto a real, safe target branch.
	if targetBranch == "" || !conductorSafeBranch(targetBranch) {
		conductorEnabled = false
	}

	// (3) Exec the opaque agent into the boundary. The grain dial (Inv 10) is read
	// from the provisioned wire: the worktree grain execs on the host in spec.Cwd
	// (unchanged); the container grain execs `container exec` INTO spec.ContainerID.
	// A container grain with no container id fails CLOSED inside spawn (RunPTY →
	// BlockedExitCode), never an ungoverned host run. The exit code (and any spawn
	// error) flows back to the caller; the deferred teardown still runs.
	code, serr := spawn(ExecTarget{
		Grain:       spec.Grain,
		Cwd:         spec.Cwd,
		ContainerID: spec.ContainerID,
	}, env, agent, agentArgs)

	// (4) AUTOMATIC CONVERGENCE (Wing 2, BOTH grains). On a CLEAN exit the boundary
	// branch is auto-converged onto the launch cwd's branch via internal/conductor,
	// BEFORE the deferred clean-exit teardown reclaims the boundary. For the WORKTREE
	// grain the gate runs in the boundary worktree against the agent's installed
	// deps; for the CONTAINER grain the conductor FETCHES the branch from the agent's
	// self-contained clone first (it is not in the canonical object store) and runs
	// the gate INSIDE the boundary container via the GateRunner seam (substrate.ExecGate)
	// — see the From/gate wiring below.
	// BEST-EFFORT / FAIL-SOFT (mirrors the cooperative layer): it NEVER alters the
	// agent's exit code or err, and a daemon-side failure must never make a governed
	// session more fragile than a bare one. A drop (code 128+signum), a non-zero
	// exit, a spawn error, or a disabled conductor all skip it.
	if conductorShouldRun(code, serr, spec.Grain, conductorEnabled) {
		// (4a) R10 converge mode (L1). MAD_CONVERGE_MODE selects how a WOULD-
		// converge clean exit is handled: auto (default/unset/unrecognized) ⇒ today's
		// L0 auto-converge; prompt ⇒ ask first on an interactive TTY (no TTY ⇒ auto,
		// never block on a missing terminal); off ⇒ skip + print the manual hint.
		// FAIL-SOFT throughout: a declined or off decision only stages the branch.
		mode := strings.ToLower(strings.TrimSpace(os.Getenv("MAD_CONVERGE_MODE")))
		dec := convergeDecision(mode, stdioIsInteractive(), conductorOff)
		if dec.ask { // prompt mode on an interactive TTY: thin I/O around the pure decision
			if promptConverge(spec.Branch, targetBranch) {
				dec.converge = true
			} else {
				dec.off = true
			}
		}
		if dec.off {
			fmt.Fprintf(os.Stderr, "mad-substrate: %s staged onto %s — run: mad-substrate integrate %s\n", spec.Branch, targetBranch, spec.Branch)
		}
		if dec.converge {
			func() {
				defer func() { _ = recover() }() // fail-soft: convergence must never crash teardown
				// FRESH connection: the session conn (sess) still has its keepalive renew
				// goroutine running, so reusing it would race a concurrent Call on one
				// connection. A bare connection is allowed to do trunk-lease ops (this
				// mirrors how `mad-substrate integrate` dials fresh, leases, and closes).
				conn, derr := dial(cfg.Socket)
				if derr != nil {
					logf("conductor: cannot dial daemon (%v); skipping auto-converge of %s", derr, spec.Branch)
					return
				}
				defer conn.Close()
				boundaryDir := spec.HostWorktree
				if boundaryDir == "" {
					boundaryDir = spec.Cwd
				}
				// Grain-specific wiring (pure, unit-tested below): the fetch source,
				// effective gate, and the GateRunner seam.
				from, gate, gateRunner := selectConductorGate(spec.Grain, spec.ContainerID, spec.HostWorktree, spec.Branch, gateCmd, logf)
				res := conductor.Converge(conn, conductor.Spec{
					Branch:       spec.Branch,
					TargetBranch: targetBranch,
					RepoDir:      launchCwd,
					BoundaryDir:  boundaryDir,
					Gate:         gate,
					From:         from,
					GateRunner:   gateRunner,
					Logf:         logf,
				})
				switch res.Status {
				case conductor.StatusConverged:
					fmt.Fprintf(os.Stderr, "mad-substrate: converged %s onto %s\n", spec.Branch, targetBranch)
				case conductor.StatusConflict:
					fmt.Fprintf(os.Stderr, "mad-substrate: %s conflicts with %s — not merged; resolve with `mad-substrate integrate %s`\n", spec.Branch, targetBranch, spec.Branch)
				case conductor.StatusGateFailed:
					fmt.Fprintf(os.Stderr, "mad-substrate: gate failed for %s — not merged (%s); fix and run `mad-substrate integrate %s`\n", spec.Branch, res.Reason, spec.Branch)
				case conductor.StatusError:
					fmt.Fprintf(os.Stderr, "mad-substrate: auto-converge unavailable (%s); run `mad-substrate integrate %s` manually\n", res.Reason, spec.Branch)
				case conductor.StatusSkipped:
					logf("conductor: skipped %s (%s)", spec.Branch, res.Reason)
				}
			}()
		}
	}
	return code, serr
}

// selectConductorGate computes the grain-specific conductor gate wiring: the fetch
// source (From), the EFFECTIVE gate string, and the GateRunner seam.
//
//   - WORKTREE grain (or "" / any non-container): a pass-through — no From, the
//     gate unchanged, and a NIL runner so the conductor uses its host `sh -c`
//     default in BoundaryDir. BYTE-IDENTICAL to before this seam existed.
//   - CONTAINER grain: From = the clone (hostWorktree) so the conductor fetches the
//     branch into the canonical store; when a gate IS configured AND a container id
//     is present, the runner executes the gate INSIDE the boundary container via
//     substrate.ExecGate (where the agent's deps live), surfacing its captured
//     output to stderr. A container grain with NO id NEVER falls back to the host
//     `sh -c` default (that would validate the host clone, not the container) — it
//     skips the gate (merge-only) instead.
//
// Pure (the only side effect is the optional logf) so the grain selection is fully
// unit-testable without a container runtime. logf may be nil.
func selectConductorGate(grain, containerID, hostWorktree, branch, gate string, logf func(string, ...any)) (from, effGate string, runner func(string) (int, []byte, error)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if grain != containerGrainName {
		return "", gate, nil // worktree / pre-grain wire: host `sh -c` default
	}
	from = hostWorktree // the clone — the conductor fetches the branch from here
	switch {
	case gate == "":
		logf("conductor: container grain — fetching %s from clone %s (merge-only)", branch, from)
		return from, "", nil
	case containerID != "":
		cid := containerID
		runner = func(g string) (int, []byte, error) {
			code, out, gErr := substrate.ExecGate(cid, g)
			if len(out) > 0 {
				// `container exec` captures rather than streams; surface the gate's
				// output the way the host PTY gate would.
				_, _ = os.Stderr.Write(out)
			}
			return code, out, gErr
		}
		logf("conductor: container grain — fetching %s from clone %s; gate runs inside container %s", branch, from, cid)
		return from, gate, runner
	default:
		// Container grain with NO container id (inconsistent wire). Do NOT fall back
		// to the host `sh -c` default — skip the gate, merge-only.
		logf("conductor: container grain — no container id; skipping gate (merge-only) for %s", branch)
		return from, "", nil
	}
}

// conductorShouldRun reports whether automatic convergence should fire after the
// agent exits. It fires on a CLEAN completion (no spawn error, exit code 0) with
// the conductor enabled — for BOTH grains: the worktree branch is already in the
// shared .git, and the container branch is fetched from the clone before the merge
// (see the conductor block / conductor.Spec.From). A drop (code 128+signum), a
// non-zero exit, a spawn error, or a disabled conductor all skip it. Pure so it is
// fully unit-tested. The grain is no longer a gate here.
func conductorShouldRun(code int, serr error, grain string, enabled bool) bool {
	_ = grain // both grains converge; retained for signature/call-site stability
	return serr == nil && code == 0 && enabled
}

// convDecision is the resolved R10 converge-mode outcome for a WOULD-converge clean
// exit: exactly one of converge (run the conductor now), ask (prompt first), or off
// (skip + print the staged manual-integrate hint).
type convDecision struct {
	converge bool
	ask      bool
	off      bool
}

// convergeDecision is the PURE R10 (L1) mode truth table. `mode` is the lowercased,
// trimmed MAD_CONVERGE_MODE; `isTTY` reports an interactive stdio; `conductorOff`
// is the legacy MAD_CONDUCTOR opt-out. Composition is "if EITHER says off, it's
// off". "auto" — the default, and any unset/empty/unrecognized value — converges, BYTE-
// IDENTICAL to the pre-R10 L0 behavior. "prompt" asks ONLY on an interactive TTY; with
// no TTY it falls back to auto (never block convergence on a missing terminal — this also
// keeps non-TTY callers, incl. the test harness, behaving exactly as today). Pure so the
// whole table is unit-tested; the prompt I/O is a thin shell around it.
func convergeDecision(mode string, isTTY bool, conductorOff bool) convDecision {
	if conductorOff || mode == "off" {
		return convDecision{off: true}
	}
	if mode == "prompt" && isTTY {
		return convDecision{ask: true}
	}
	return convDecision{converge: true}
}

// stdioIsInteractive reports whether BOTH stdin and stdout are terminals — the
// precondition for a meaningful interactive converge prompt. Reuses golang.org/x/term
// (already a launcher dep via pty.go); no new dependency.
func stdioIsInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// promptConverge prints the L1 "converge now?" prompt and reads one line. Empty / yes
// ⇒ true (converge); "n"/"no" ⇒ false (stage). FAIL-SOFT: a read error returns true so
// a flaky terminal never strands the agent's work. Only called when stdio is a TTY.
func promptConverge(branch, target string) bool {
	fmt.Fprintf(os.Stderr, "mad-substrate: converge %s onto %s? [Y/n] ", branch, target)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return true // fail-soft: never strand work on a prompt read error
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "n", "no":
		return false
	default: // yes, empty, or anything unrecognized → converge ([Y/n] default)
		return true
	}
}

// gitLine runs `git -C dir args...` and returns its trimmed stdout. Read-only
// uses only (rev-parse) — it never mutates the repo.
func gitLine(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// conductorSafeBranch reports whether a branch name is safe to converge onto:
// non-empty, NOT starting with '-' (so git can never parse it as a flag), and
// only [A-Za-z0-9._/-]. This mirrors the conductor's own safeBranchName guard — a
// defensive double-check at the launcher boundary against git arg-injection.
func conductorSafeBranch(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
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

// coopHost maps a requested agent to the cooperative host whose MCP/hook config
// `launch` knows how to generate, or "" for an agent we do not wire (a shell, a
// custom tool). It keys on the same names as the transparent shim (SupportedAgents)
// so an agent that can be shimmed is also one we wire cooperatively.
func coopHost(agent string) string {
	switch strings.ToLower(filepath.Base(strings.TrimSpace(agent))) {
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	default:
		return ""
	}
}

// wireContainerMCP stages the embedded static linux mad-substrate binary into the
// container's writable scratch dir and generates the agent's MCP config against the
// IN-CONTAINER CLONE (hostWorktree, bind-mounted at /work), with the MCP server
// `command` pointed at the STAGED in-container mad-substrate path + arg "mcp". The scratch
// dir is bind-mounted at its IDENTICAL host path in the container, so the staged HOST
// path IS the in-container exec path the agent will run. Returns the agent ExtraArgs
// (Codex `-c` overrides; empty for Claude) plus the coopwiring Result.
//
// FAIL-SOFT: it returns the FIRST hard error for the caller to LOG and never blocks the
// launch itself. An empty hostWorktree is an error (no real clone to wire). host must
// be a recognized cooperative host ("claude"|"codex"); an unrecognized host yields a
// zero Result and nil error (coopwiring.Wire's contract). Pure except for the staging
// filesystem write + coopwiring's config write/git-exclude, so it is unit-testable with
// a temp git repo + temp scratch and an injected madSubstrateBytesFn — no container runtime.
func wireContainerMCP(host, hostWorktree, scratchDir, goarch string, logf func(string, ...any)) (coopwiring.Result, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if strings.TrimSpace(hostWorktree) == "" {
		return coopwiring.Result{}, fmt.Errorf("no in-container clone path to wire")
	}
	staged, err := stageMadSubstrate(scratchDir, goarch)
	if err != nil {
		return coopwiring.Result{}, fmt.Errorf("stage in-container mad-substrate: %w", err)
	}
	logf("coop: staged in-container mad-substrate at %s", staged)
	return coopwiring.Wire(host, hostWorktree, staged)
}

// grainLabel renders a boundary grain for diagnostics ("" is the pre-grain wire,
// which is the host worktree).
func grainLabel(g string) string {
	if g == "" {
		return "worktree"
	}
	return g
}
