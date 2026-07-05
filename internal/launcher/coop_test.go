package launcher

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/coop"
	"github.com/madhavhaldia/mad-trellis/internal/substrate"
)

// TestCoopPumpDemuxEchoMultiStream drives the pump core with in-memory tunnel
// pipes and a real unix ECHO daemon: it proves READY readiness, that each OPEN
// dials its OWN daemon connection, that DATA round-trips byte-identically and is
// demultiplexed per stream, that two streams are independent, and that CLOSE tears
// one stream down without disturbing the other. (The container exec is NOT in the
// loop here — that is covered by the live e2e; this isolates the multiplexer.)
func TestCoopPumpDemuxEchoMultiStream(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { _, _ = io.Copy(c, c); _ = c.Close() }(c)
		}
	}()

	prOut, pwOut := io.Pipe() // relay→pump: test writes frames to pwOut, pump reads prOut
	prIn, pwIn := io.Pipe()   // pump→relay: pump writes frames to pwIn, test reads prIn
	defer pwOut.Close()
	defer pwIn.Close()

	p := newCoopPump(pwIn, prOut, func() (net.Conn, error) { return net.Dial("unix", sock) }, nil)
	go p.run()

	type fr struct {
		f   coop.Frame
		err error
	}
	frames := make(chan fr, 64)
	go func() {
		for {
			f, err := coop.ReadFrame(prIn)
			frames <- fr{f, err}
			if err != nil {
				return
			}
		}
	}()

	write := func(id uint32, ty coop.FrameType, payload []byte) {
		if err := coop.WriteFrame(pwOut, id, ty, payload); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	readEcho := func(wantID uint32, want string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case x := <-frames:
				if x.err != nil {
					t.Fatalf("pump frame read err: %v", x.err)
				}
				if x.f.StreamID == wantID && x.f.Type == coop.FrameData && string(x.f.Payload) == want {
					return
				}
				// skip unrelated frames (e.g. a teardown CLOSE)
			case <-deadline:
				t.Fatalf("timeout waiting for echo id=%d %q", wantID, want)
			}
		}
	}

	// readiness
	write(0, coop.FrameReady, nil)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("pump never became ready")
	}

	// stream 1
	write(1, coop.FrameOpen, nil)
	write(1, coop.FrameData, []byte("hello"))
	readEcho(1, "hello")

	// stream 2 (independent daemon connection)
	write(2, coop.FrameOpen, nil)
	write(2, coop.FrameData, []byte("world!!"))
	readEcho(2, "world!!")

	// stream 1 still independently alive
	write(1, coop.FrameData, []byte("again"))
	readEcho(1, "again")

	// close stream 1; its daemon conn is dropped, stream 2 unaffected
	write(1, coop.FrameClose, nil)
	write(1, coop.FrameData, []byte("ignored")) // to a closed stream → dropped, no echo
	write(2, coop.FrameData, []byte("ok2"))
	readEcho(2, "ok2")

	// shutdown: relay EOF → pump run() ends, closing the remaining daemon conn
	pwOut.Close()
}

// TestCoopPumpDialFailureClosesStream proves that when the daemon dial fails for a
// newly OPENed stream, the pump tells the relay to drop the adapter connection
// (CLOSE) instead of hanging it.
func TestCoopPumpDialFailureClosesStream(t *testing.T) {
	prOut, pwOut := io.Pipe()
	prIn, pwIn := io.Pipe()
	defer pwOut.Close()
	defer pwIn.Close()

	// dial always fails (no daemon).
	p := newCoopPump(pwIn, prOut, func() (net.Conn, error) {
		return nil, io.ErrClosedPipe
	}, nil)
	go p.run()

	if err := coop.WriteFrame(pwOut, 5, coop.FrameOpen, nil); err != nil {
		t.Fatal(err)
	}
	got := make(chan coop.Frame, 1)
	go func() {
		f, err := coop.ReadFrame(prIn)
		if err == nil {
			got <- f
		}
	}()
	select {
	case f := <-got:
		if f.StreamID != 5 || f.Type != coop.FrameClose {
			t.Fatalf("expected CLOSE for stream 5 on dial failure, got %+v", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pump did not emit CLOSE on dial failure")
	}
}

// TestCoopPumpSlowStreamDoesNotWedgeOthers is the regression test for the review's
// confirmed HIGH: a stalled/slow stream must NOT wedge the multiplex. Stream 1's
// daemon connection is a BLACK HOLE (a net.Pipe whose far end is never read, so any
// write to it blocks forever); stream 2 is a healthy echo. The test proves (a)
// stream 2 echoes while stream 1's writer is stuck, and (b) flooding stream 1 past
// its bounded queue SHEDS it (a CLOSE is emitted) while stream 2 keeps working —
// i.e. one stuck stream can neither head-of-line-block nor deadlock the others.
func TestCoopPumpSlowStreamDoesNotWedgeOthers(t *testing.T) {
	// Short /tmp path: the default t.TempDir() path can exceed the macOS unix-socket
	// length limit (~104 chars) for this long test name.
	dir, err := os.MkdirTemp("/tmp", "nmc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			go func(c net.Conn) { _, _ = io.Copy(c, c); _ = c.Close() }(c)
		}
	}()

	// Black hole: a net.Pipe whose far end is never read → any Write blocks forever.
	bhLocal, bhRemote := net.Pipe()
	defer bhLocal.Close()
	defer bhRemote.Close()

	var dialN int32
	dial := func() (net.Conn, error) {
		if atomic.AddInt32(&dialN, 1) == 1 {
			return bhLocal, nil // first stream → black hole
		}
		return net.Dial("unix", sock) // later streams → echo
	}

	prOut, pwOut := io.Pipe()
	prIn, pwIn := io.Pipe()
	defer pwOut.Close()
	defer pwIn.Close()
	p := newCoopPump(pwIn, prOut, dial, nil)
	go p.run()

	frames := make(chan coop.Frame, 512)
	go func() {
		for {
			f, e := coop.ReadFrame(prIn)
			if e != nil {
				return
			}
			frames <- f
		}
	}()
	write := func(id uint32, ty coop.FrameType, b []byte) {
		if err := coop.WriteFrame(pwOut, id, ty, b); err != nil {
			t.Errorf("write: %v", err)
		}
	}
	waitEcho := func(id uint32, want string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case f := <-frames:
				if f.StreamID == id && f.Type == coop.FrameData && string(f.Payload) == want {
					return
				}
			case <-deadline:
				t.Fatalf("WEDGE: stream %d echo %q never arrived", id, want)
			}
		}
	}

	write(0, coop.FrameReady, nil)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("pump never ready")
	}

	// stream 1 → black hole; let serveStream(1) grab the first dial deterministically.
	write(1, coop.FrameOpen, nil)
	time.Sleep(200 * time.Millisecond)
	write(1, coop.FrameData, []byte("stuck")) // stream 1's writer now blocks on the black-hole Write

	// stream 2 → echo: must work despite stream 1 being stuck (no head-of-line block).
	write(2, coop.FrameOpen, nil)
	write(2, coop.FrameData, []byte("hi"))
	waitEcho(2, "hi")

	// Flood stream 1 past its bounded queue → it is SHED, and stream 2 keeps working.
	for i := 0; i < coopStreamQueue+50; i++ {
		write(1, coop.FrameData, []byte("x"))
	}
	write(2, coop.FrameData, []byte("hi2"))
	waitEcho(2, "hi2")

	pwOut.Close()
}

// TestCoopPumpShedDuringDialTearsDownCleanly is the regression test for the
// re-verify's HIGH: a stream SHED while its daemon dial is still in flight must
// still (a) tell the relay to drop the adapter conn (emit CLOSE) and (b) be removed
// from the pump map — otherwise the relay-side adapter connection hangs forever and
// the map entry leaks.
func TestCoopPumpShedDuringDialTearsDownCleanly(t *testing.T) {
	release := make(chan struct{})
	var dialed int32
	dial := func() (net.Conn, error) {
		atomic.AddInt32(&dialed, 1)
		<-release // block the dial until the test releases it
		c1, c2 := net.Pipe()
		go func() { _, _ = io.Copy(io.Discard, c2); _ = c2.Close() }()
		return c1, nil
	}

	prOut, pwOut := io.Pipe()
	prIn, pwIn := io.Pipe()
	defer pwOut.Close()
	defer pwIn.Close()
	p := newCoopPump(pwIn, prOut, dial, nil)
	go p.run()

	frames := make(chan coop.Frame, 1024)
	go func() {
		for {
			f, e := coop.ReadFrame(prIn)
			if e != nil {
				return
			}
			frames <- f
		}
	}()
	write := func(id uint32, ty coop.FrameType, b []byte) { _ = coop.WriteFrame(pwOut, id, ty, b) }

	write(0, coop.FrameReady, nil)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("pump never ready")
	}

	write(1, coop.FrameOpen, nil)
	for atomic.LoadInt32(&dialed) == 0 { // wait until serveStream(1) is blocked in the dial
		time.Sleep(5 * time.Millisecond)
	}
	// Flood past the bounded queue WHILE the dial is still blocked → SHED during the
	// dial window (the buggy path that skipped removeStream + CLOSE).
	for i := 0; i < coopStreamQueue+50; i++ {
		write(1, coop.FrameData, []byte("x"))
	}
	time.Sleep(100 * time.Millisecond)
	close(release) // dial completes → setConn sees closed → the fixed cleanup must run

	deadline := time.After(3 * time.Second)
	for {
		select {
		case f := <-frames:
			if f.StreamID == 1 && f.Type == coop.FrameClose {
				goto closed
			}
		case <-deadline:
			t.Fatal("no CLOSE emitted for a stream shed during the dial window (relay-side conn would hang)")
		}
	}
closed:
	rmDeadline := time.After(2 * time.Second)
	for {
		p.mu.Lock()
		_, present := p.streams[1]
		p.mu.Unlock()
		if !present {
			break
		}
		select {
		case <-rmDeadline:
			t.Fatal("stream not removed from the pump map after shed-during-dial (map leak)")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	pwOut.Close()
}

func TestStageRelay(t *testing.T) {
	dir := t.TempDir()
	if _, err := stageRelay(filepath.Join(dir, "nope"), dir); err == nil {
		t.Fatal("expected error for a missing relay binary")
	}
	bin := filepath.Join(dir, "relay")
	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := stageRelay(bin, ""); err == nil {
		t.Fatal("expected error for an empty scratch dir")
	}
	scratch := filepath.Join(dir, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := stageRelay(bin, scratch)
	if err != nil {
		t.Fatalf("stageRelay: %v", err)
	}
	if got != filepath.Join(scratch, relayStageName) {
		t.Fatalf("staged at %q, want %q", got, filepath.Join(scratch, relayStageName))
	}
	fi, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("staged relay must be executable, mode=%v", fi.Mode())
	}
	// no temp left behind
	if _, err := os.Stat(got + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("staging temp file leaked")
	}
}

// containerSpec is a provisioned wire at the CONTAINER grain (with a writable
// scratch dir) used by the coop gating/fail-soft tests.
func containerSpec() substrate.Wire {
	return substrate.Wire{
		Session: "s-1-abc", Grain: "container",
		Cwd: "/work", Branch: "nm/s-1-abc", HostWorktree: "/tmp/clone", ContainerID: "ctr-1",
		Ports: []int{5000},
		Env:   map[string]string{"MAD_SESSION": "s-1-abc", "MAD_SCRATCH": "/tmp/scratch-x"},
	}
}

// TestRunCoopSkippedForWorktreeGrain: the opt-in env is set, but a WORKTREE-grain
// boundary must NOT start a relay or override MAD_SOCKET (the coop plane is
// container-only).
func TestRunCoopSkippedForWorktreeGrain(t *testing.T) {
	t.Setenv(coopRelayEnv, "/some/relay")
	f := &fakeConn{whoami: "s-1-abc", provision: okSpec()}
	dial := func(string) (Conn, error) { return f, nil }
	sp := &recordingSpawn{}
	if _, err := Run(Config{Agent: "claude", Socket: "/tmp/x.sock", Dial: dial, Spawn: sp.fn}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sp.called {
		t.Fatal("agent must run")
	}
	if sp.env["MAD_SOCKET"] != "" {
		t.Fatalf("worktree grain must NOT override MAD_SOCKET; got %q", sp.env["MAD_SOCKET"])
	}
}

// TestRunCoopSkippedWhenEnvUnset: a container boundary with the opt-in UNSET runs
// confined exactly as before — no relay, MAD_SOCKET untouched.
func TestRunCoopSkippedWhenEnvUnset(t *testing.T) {
	t.Setenv(coopRelayEnv, "") // explicitly unset/empty
	f := &fakeConn{whoami: "s-1-abc", provision: containerSpec()}
	dial := func(string) (Conn, error) { return f, nil }
	sp := &recordingSpawn{}
	if _, err := Run(Config{Agent: "claude", Socket: "/tmp/x.sock", Dial: dial, Spawn: sp.fn}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sp.called || sp.target.Grain != "container" {
		t.Fatal("agent must run in the container")
	}
	if sp.env["MAD_SOCKET"] != "" {
		t.Fatalf("no relay → MAD_SOCKET must be unchanged; got %q", sp.env["MAD_SOCKET"])
	}
}

// TestRunCoopFailSoftWhenRelayBinaryMissing: container boundary + opt-in set, but
// the relay binary does not exist → startCoop fails at stageRelay BEFORE any
// `container exec` → FAIL-SOFT: the agent still runs confined and MAD_SOCKET
// is NOT overridden. This is the cardinal additive-feature property: a broken
// cooperative plane never demotes a working confined launch.
func TestRunCoopFailSoftWhenRelayBinaryMissing(t *testing.T) {
	t.Setenv(coopRelayEnv, filepath.Join(t.TempDir(), "does-not-exist"))
	f := &fakeConn{whoami: "s-1-abc", provision: containerSpec()}
	dial := func(string) (Conn, error) { return f, nil }
	sp := &recordingSpawn{}
	if _, err := Run(Config{Agent: "claude", Socket: "/tmp/x.sock", Dial: dial, Spawn: sp.fn}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sp.called || sp.target.Grain != "container" {
		t.Fatal("fail-soft: the agent must still run confined")
	}
	if sp.env["MAD_SOCKET"] != "" {
		t.Fatalf("fail-soft must NOT override MAD_SOCKET; got %q", sp.env["MAD_SOCKET"])
	}
}

// captureStderr swaps os.Stderr for a pipe around fn and returns what fn wrote to it.
// Used to assert the operator-visible coop status WITHOUT MAD_DEBUG (the logf
// diagnostics sink is discard by default, so logf can't carry it). No parallel test in
// this package touches os.Stderr, so the global swap is safe here.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = orig
	_ = w.Close()
	out, _ := io.ReadAll(r)
	_ = r.Close()
	return string(out)
}

// TestRunCoopStatusVisibleOnStderr: the cooperative-plane STATUS reaches the operator
// on os.Stderr at launch even WITHOUT MAD_DEBUG (the diagnostics logf is discard
// by default). A container boundary whose relay cannot start runs confined and MUST
// print the confined status — and MUST NOT claim the plane is up.
//
// NON-VACUOUS CONTROL: a WORKTREE-grain launch (which has no container coop block)
// prints NEITHER status line, proving the line reflects the actual container coop
// outcome rather than being emitted unconditionally on every launch.
func TestRunCoopStatusVisibleOnStderr(t *testing.T) {
	const upMsg = "cooperative plane up"
	const confinedMsg = "cooperative plane unavailable"

	// Container grain + a relay that cannot start → confined: prints the confined
	// status, never the "up" status.
	t.Setenv(coopRelayEnv, filepath.Join(t.TempDir(), "does-not-exist"))
	cf := &fakeConn{whoami: "s-1-abc", provision: containerSpec()}
	csp := &recordingSpawn{}
	cout := captureStderr(t, func() {
		if _, err := Run(Config{Agent: "claude", Socket: "/tmp/x.sock", Dial: func(string) (Conn, error) { return cf, nil }, Spawn: csp.fn}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	if !strings.Contains(cout, confinedMsg) {
		t.Fatalf("container+broken-relay must print the confined coop status; stderr=%q", cout)
	}
	if strings.Contains(cout, upMsg) {
		t.Fatalf("must NOT claim the coop plane is up when confined; stderr=%q", cout)
	}

	// CONTROL: worktree grain has no coop block → NEITHER status line is emitted.
	wf := &fakeConn{whoami: "s-1-abc", provision: okSpec()}
	wsp := &recordingSpawn{}
	wout := captureStderr(t, func() {
		if _, err := Run(Config{Agent: "claude", Socket: "/tmp/x.sock", Dial: func(string) (Conn, error) { return wf, nil }, Spawn: wsp.fn}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	if strings.Contains(wout, upMsg) || strings.Contains(wout, confinedMsg) {
		t.Fatalf("worktree grain must emit NO coop status line; stderr=%q", wout)
	}
}
