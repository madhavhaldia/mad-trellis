package launcher

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// dropGrace bounds how long runPTYIO waits for the child to die AFTER it relays a
// terminal-drop signal (SIGHUP/SIGINT/SIGTERM) before it returns ANYWAY so the
// launcher's bounded clean-exit teardown (Run's defer) can run. The drop path —
// the controlling terminal closes (SIGHUP) or the controlling process dies — must
// NOT depend on the child actually terminating: an agent that traps/ignores the
// signal, or an in-container child the relayed signal does not promptly reach,
// would otherwise leave runPTYIO blocked in Wait() forever, so Run never returns
// and its teardown (lease release + boundary reclaim) never fires — the orphaned
// worktree/reservation bug. We give the child a short grace to exit cleanly (so a
// well-behaved agent still yields its EXACT exit code), then return regardless.
// Kept small so a wedged child cannot delay teardown; the teardown itself is
// independently bounded by cleanExitTimeout.
var dropGrace = 3 * time.Second

// RunPTY runs the agent on a real PTY, with the substrate's env-spec merged over
// the inherited environment, so a governed session is byte-for-byte
// indistinguishable from a bare one (Inv 13 ambient interaction): the user's own
// terminal is put in raw mode and bridged to the child's PTY, window resizes
// propagate (SIGWINCH), and termination/stop signals delivered to the launcher
// are forwarded to the child. It returns the child's EXACT exit status (128+signum
// on signal death, the POSIX convention) so the wrapper is transparent to a
// caller/script.
//
// The GRAIN dial (Inv 10) is read from `target`: the default worktree grain execs
// the agent on the HOST in target.Cwd (byte-identical to the pre-grain path); the
// container grain execs `container exec` INTO the confined container, and the SAME
// PTY plumbing wraps that process so signal forwarding and the exact exit code
// flow through to the in-container agent. It is the production SpawnFunc; the
// IO-injected core (runPTYIO) makes the parity contract testable.
func RunPTY(target ExecTarget, extraEnv map[string]string, agent string, args []string) (int, error) {
	return runPTYIO(os.Stdin, os.Stdout, target, extraEnv, agent, args)
}

// runPTYIO is RunPTY with the in/out streams injected, so the ambient-interaction
// parity (env applied, args verbatim, exact exit code) can be asserted against a
// real PTY in tests without commandeering the process's os.Stdin/os.Stdout. The
// tty-only behaviour (raw mode + resize) engages only when `stdin` is an *os.File
// that is actually a terminal. The host-vs-container exec choice is factored into
// buildExecCommand; a container grain with no container id fails CLOSED here
// (BlockedExitCode), never an ungoverned host run.
func runPTYIO(stdin io.Reader, stdout io.Writer, target ExecTarget, extraEnv map[string]string, agent string, args []string) (int, error) {
	return runPTYIOWithOptions(stdin, stdout, target, extraEnv, agent, args, ptyRunOptions{})
}

func runPTYIOWithOptions(stdin io.Reader, stdout io.Writer, target ExecTarget, extraEnv map[string]string, agent string, args []string, opts ptyRunOptions) (int, error) {
	c, err := buildExecCommand(target, extraEnv, agent, args)
	if err != nil {
		return BlockedExitCode, err
	}

	ptmx, err := pty.Start(c)
	if err != nil {
		return BlockedExitCode, err
	}
	defer func() { _ = ptmx.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Raw mode + window-size propagation, only when our stdin is a real terminal.
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				_ = pty.InheritSize(f, ptmx)
			}
		}()
		winch <- syscall.SIGWINCH // initial sizing
		if old, rerr := term.MakeRaw(int(f.Fd())); rerr == nil {
			defer func() { _ = term.Restore(int(f.Fd()), old) }()
		}
	}

	// Forward process-directed termination/stop signals to the child. This is
	// load-bearing for clean-exit teardown (Inv 4): once these are caught, a
	// signal sent to the LAUNCHER process (kill -INT/-TERM, a non-tty parent, a CI
	// wrapper) no longer kills it by DEFAULT DISPOSITION — which would skip the
	// deferred teardown and orphan the child. Instead we relay the signal to the
	// child. Interactive Ctrl-C/Ctrl-Z still reach the child as raw bytes via the
	// PTY line discipline (ISIG is off on our terminal in raw mode), so there is no
	// double-delivery.
	//
	// A TERMINAL-DROP signal (SIGHUP when the controlling terminal closes or the
	// controlling process dies; also SIGINT/SIGTERM) is special: relaying it is not
	// enough, because the agent may trap/ignore it, or — at the container grain —
	// the relayed signal may not promptly reach the in-container child. We therefore
	// also ARM a bounded grace (dropGrace) and RETURN once the child exits OR the
	// grace elapses, whichever first. That guarantees runPTYIO returns on the drop
	// path so Run's deferred (and independently bounded) clean-exit teardown fires —
	// closing the orphaned-worktree/reservation leak. A well-behaved child that exits
	// within the grace still yields its EXACT exit code; only a wedged child returns
	// the conventional 128+signum so the launcher always makes progress.
	fwd := make(chan os.Signal, 1)
	signal.Notify(fwd, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTSTP, syscall.SIGCONT)
	defer signal.Stop(fwd)
	dropped := make(chan os.Signal, 1) // the first terminal-drop signal seen
	go func() {
		for s := range fwd {
			if c.Process != nil {
				if sig, ok := s.(syscall.Signal); ok {
					_ = c.Process.Signal(sig)
				}
			}
			if isTerminalDrop(s) {
				select {
				case dropped <- s:
				default: // already armed; one grace timer is enough
				}
			}
		}
	}()

	var ptmxMu sync.Mutex
	activity := &inputActivity{}
	ptmxWriter := lockedWriter{mu: &ptmxMu, w: ptmx}
	go func() { _, _ = io.Copy(ptmxWriter, activityReader{r: stdin, activity: activity}) }()
	// Nudges deliver through the injector (inject.go): body as a (bracketed-paste-
	// aware) insert, then a temporally isolated \r submit — under the SAME ptmxMu
	// as the stdin relay so injected input and user keystrokes never interleave.
	paste := &pasteModeTracker{}
	startNudgeLoop(ctx, &ptyInjector{mu: &ptmxMu, w: ptmx, paste: paste, delay: opts.Nudges.SubmitDelay}, activity, opts.Nudges)

	// Drain the child's output. On the NORMAL path this returns when the child exits
	// (the PTY master sees EOF), so it fully flushes stdout BEFORE we return — the
	// transparency contract (no truncated transcript). We run it in a goroutine only
	// so the DROP path can return without it (a wedged child never closes the PTY);
	// the normal path still WAITS for it via outDone below. The paste tracker tees
	// off the SAME relay so the injector sees the child's bracketed-paste mode.
	outDone := make(chan struct{})
	go func() { _, _ = io.Copy(stdout, io.TeeReader(ptmx, paste)); close(outDone) }()

	// Wait for the child in the background so the drop path can return without it.
	waitErr := make(chan error, 1)
	go func() { waitErr <- c.Wait() }()

	select {
	case err := <-waitErr:
		// Normal path: the child exited (clean, non-zero, or signal death). Flush the
		// remaining output before returning, then propagate the EXACT code.
		<-outDone
		return waitCode(err)
	case s := <-dropped:
		// Terminal-drop path: the launcher was told to go away. Give the child a
		// bounded grace to exit cleanly (preserving its exact code), then return
		// REGARDLESS so Run's clean-exit teardown runs — even if the child wedged.
		select {
		case err := <-waitErr:
			<-outDone // the child exited within grace: flush, then exact code
			return waitCode(err)
		case <-time.After(dropGrace):
			// The child did not exit in time. Return the conventional signal-death
			// code for the drop signal and let teardown reclaim the boundary; the
			// process is exiting, so the (still-running) child is reaped by init.
			sig := syscall.SIGHUP
			if ss, ok := s.(syscall.Signal); ok {
				sig = ss
			}
			return 128 + int(sig), nil
		}
	}
}

// isTerminalDrop reports whether s is a session-drop / termination signal — the
// launcher being told to go away (SIGHUP = controlling terminal closed or the
// controlling process died; SIGINT/SIGTERM = an explicit kill). These drive the
// bounded teardown-on-drop path; job-control / continuation signals (SIGTSTP,
// SIGCONT, SIGQUIT) are merely relayed to the child and do NOT trigger teardown.
func isTerminalDrop(s os.Signal) bool {
	switch s {
	case syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM:
		return true
	default:
		return false
	}
}

// waitCode maps the result of exec.Cmd.Wait to a process exit code. A clean exit
// returns its code; signal death returns 128+signum (POSIX); any other error is
// reported as the generic failure code so the launcher never hides a non-zero
// outcome behind a 0.
func waitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal()), nil
			}
			return ws.ExitStatus(), nil
		}
		return ee.ExitCode(), nil
	}
	return 1, err
}

// MergeEnv overlays the substrate's env-spec onto a base environment: a key in
// `over` replaces any same-named base entry (so the per-agent PORT/TMPDIR/XDG/
// MAD_* values win), then the remaining base entries pass through. The
// substrate's env-spec deliberately never names a launch-critical variable
// (PATH/LD_*/…) — those are validated out at provision time — so the child
// inherits the launcher's PATH and toolchain unchanged. Deterministic ordering.
func MergeEnv(base []string, over map[string]string) []string {
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			if _, overridden := over[kv[:eq]]; overridden {
				continue
			}
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(over))
	for k := range over {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+over[k])
	}
	return out
}
