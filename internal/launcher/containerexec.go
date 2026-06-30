package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// errMissingContainerID is the fail-closed condition for an inconsistent wire: the
// boundary grain is "container" but no container id was provisioned. RunPTY maps it
// to BlockedExitCode — the agent is NEVER run on the host as a silent fallback.
var errMissingContainerID = errors.New("container grain selected but no container id on the boundary; refusing to fall back to an ungoverned host run")

// containerGrainName is the boundary grain that means "the agent must run INSIDE
// a confined container" (substrate's container.go). It MUST match the substrate's
// grain name on the wire; any other value (including "" / "worktree") takes the
// host path unchanged.
const containerGrainName = "container"

// containerWorkMount mirrors the substrate's in-container bind-mount point for the
// agent's worktree. The container-exec path sets the working dir to this so the
// agent lands in its governed workspace regardless of the spec.Cwd projection.
const containerWorkMount = "/work"

// containerBin is the runtime CLI the grain conducts. Kept as a package var so a
// test can point it at a fake without a real runtime (the production value is the
// verified "container" CLI, resolved on PATH).
var containerBin = "container"

// ExecTarget tells the PTY core WHERE to exec the agent: the host (worktree grain,
// the default — BYTE-IDENTICAL to before this seam existed) or INSIDE a confined
// container (the T3 grainswap). It is derived from the substrate Wire; an empty
// Grain or a missing ContainerID always selects the host path, so the worktree
// grain can never be diverted into a container by accident (fail-safe to host
// here; fail-CLOSED on a container-exec that cannot start is enforced in pty.go).
type ExecTarget struct {
	Grain       string // boundary grain ("" / "worktree" → host; "container" → in-container)
	Cwd         string // host-grain working dir (the worktree path)
	ContainerID string // the confined container id to exec into (container grain only)
}

// errUnknownGrain is the fail-closed condition for a boundary grain the launcher
// does not recognize: rather than silently running the agent on the HOST (the old
// fall-through), an unknown/future/typo grain is BLOCKED. RunPTY maps it to
// BlockedExitCode — the agent is never reached on the host as a silent fallback.
var errUnknownGrain = errors.New("unknown boundary grain; refusing to fall back to an ungoverned host run")

// hostGrainName / worktreeGrainName are the grains that run the agent on the HOST
// (the default — BYTE-IDENTICAL to before this seam existed). "" is the pre-grain
// wire (no grain field); "worktree" is the v1 grain whose workspace IS the host.
const (
	hostGrainName     = ""
	worktreeGrainName = "worktree"
)

// buildExecCommand constructs the *exec.Cmd the PTY core wraps, picking host-exec
// vs container-exec from the target. The returned command's Process is what the
// PTY plumbing drives (raw-mode IO bridge + signal forwarding + exact exit code);
// for the container path that process is the `container exec` client, and the
// signals/exit it carries are the in-container agent's (the runtime relays them).
//
//   - HOST (worktree grain, default): exec.Command(agent, args) in cwd with the
//     merged env — IDENTICAL to the pre-grain behavior, byte for byte.
//   - CONTAINER: container exec -i -t -w /work [-e K=V ...] <id> <agent> <args...>.
//     The governed env-spec (MAD_SESSION, ports, state dirs, and — when
//     present — MAD_SESSION_TOKEN) is passed via -e so the in-container agent
//     sees the SAME governed environment a host-grain agent would. The launcher's
//     own PATH/toolchain is NOT forwarded: the image provides the in-container PATH
//     (mirroring the substrate's rule that the env-spec never names PATH).
//
// ROUTING is an EXPLICIT ALLOWLIST (never a fall-through): "" / "worktree" → host
// exec; "container" → container exec (empty ContainerID is the fail-closed
// errMissingContainerID); ANY OTHER grain → errUnknownGrain (fail-closed). An
// unknown/future/typo grain is NEVER silently run on the host. buildExecCommand is
// the SOLE routing authority (there is no other isContainer-style predicate). A
// "container" grain with an empty ContainerID returns an error (fail-closed); the
// caller (RunPTY) maps any error here to BlockedExitCode, never an ungoverned host run.
func buildExecCommand(target ExecTarget, env map[string]string, agent string, args []string) (*exec.Cmd, error) {
	switch target.Grain {
	case hostGrainName, worktreeGrainName:
		// HOST PATH — byte-identical to the original RunPTY exec.
		c := exec.Command(agent, args...)
		c.Dir = target.Cwd
		c.Env = MergeEnv(os.Environ(), env)
		return c, nil
	case containerGrainName:
		if target.ContainerID == "" {
			return nil, errMissingContainerID
		}
		return buildContainerExec(target.ContainerID, env, agent, args), nil
	default:
		return nil, fmt.Errorf("%w: %q", errUnknownGrain, target.Grain)
	}
}

// buildContainerExec assembles the `container exec` invocation that runs the agent
// inside the confined container at /work with the governed env-spec injected. The
// container-exec process inherits the LAUNCHER's environment (so PATH locates the
// `container` CLI); the AGENT's governed env is carried only via -e flags so it is
// applied inside the container, not leaked into the runtime client. Env keys are
// sorted for a deterministic, testable argv.
func buildContainerExec(containerID string, env map[string]string, agent string, args []string) *exec.Cmd {
	argv := []string{"exec", "-i", "-t", "-w", containerWorkMount}
	// NON-ROOT EXEC USER (the `claude --dangerously-skip-permissions` enabler): claude
	// REFUSES that flag when running as ROOT, so MAD_CONTAINER_USER lets the agent
	// be exec'd as a non-root user — a bare name OR a "uid:gid" pair. UNSET → NO --user
	// (byte-identical to today's root default); an UNSAFE value is IGNORED (never
	// arg-injected). NOTE (live requirement, out of scope for the unit test): the chosen
	// non-root user MUST be able to READ the mounted credentials (containerHome/.claude,
	// ~/.codex) and /work — i.e. the host→container uid mapping must line up so the user
	// owns those paths. That holds only against a real runtime and is validated live.
	if u := os.Getenv("MAD_CONTAINER_USER"); isSafeContainerUser(u) {
		argv = append(argv, "--user", u)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "-e", k+"="+env[k])
	}
	argv = append(argv, containerID, agent)
	argv = append(argv, args...)
	return exec.Command(containerBin, argv...)
}

// isSafeContainerUser reports whether an ENV-sourced container user is safe to pass as
// the `container exec --user` value: NON-empty, NO leading '-' (so it can never be
// parsed as a flag — the arg-injection guard), and only [A-Za-z0-9._:-] so a bare name
// OR a "uid:gid" pair is accepted and nothing else. An unsafe value is IGNORED by
// buildContainerExec (no --user is added), never forwarded to the runtime.
func isSafeContainerUser(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == ':' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}
