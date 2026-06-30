package launcher

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/coop"
	"github.com/madhavhaldia/mad-substrate/internal/coopembed"
)

// relayBytesFn resolves the embedded static linux relay payload for an arch. It is
// indirected through a package var ONLY so the auto-resolve tests can inject an
// embedded payload without a -tags coopembed build (the untagged stub embeds
// nothing). Production = coopembed.RelayBytes.
var relayBytesFn = coopembed.RelayBytes

// madSubstrateBytesFn resolves the embedded static linux mad-substrate binary for an arch. It
// is indirected through a package var ONLY so the in-container MCP-wiring tests can
// inject a payload without a -tags coopembed build (the untagged stub embeds nothing).
// Production = coopembed.MadSubstrateBytes.
var madSubstrateBytesFn = coopembed.MadSubstrateBytes

// resolveRelayHostPath resolves the host path of the static linux relay binary to
// stage into the container, AUTO-RESOLVING in precedence order:
//
//  1. MAD_CONTAINER_RELAY (the OVERRIDE): an explicit host path, returned
//     as-is with a NIL cleanup (the caller does not own that file). Unchanged from
//     the original opt-in knob.
//  2. The EMBEDDED relay (coopembed — compiled in ONLY by a -tags coopembed build):
//     the bytes for goarch are written to a 0755 temp host file and that path is
//     returned with a cleanup that removes it. This is what makes the cooperative
//     plane ON BY DEFAULT for the container grain — no env needed.
//
// Returns ("", nil, nil) when NEITHER source is available (an untagged build with no
// override): the caller treats that as fail-soft — run the agent confined without
// the plane. A non-nil error is only an embedded-write failure (also fail-soft at
// the caller). NEVER blocks the agent.
func resolveRelayHostPath(goarch string) (path string, cleanup func(), err error) {
	if p := strings.TrimSpace(os.Getenv(coopRelayEnv)); p != "" {
		return p, nil, nil
	}
	b, ok := relayBytesFn(goarch)
	if !ok {
		return "", nil, nil
	}
	f, err := os.CreateTemp("", "mad-substrate-relay-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp relay: %w", err)
	}
	name := f.Name()
	if _, werr := f.Write(b); werr != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", nil, fmt.Errorf("write embedded relay: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(name)
		return "", nil, fmt.Errorf("close embedded relay: %w", cerr)
	}
	if cerr := os.Chmod(name, 0o755); cerr != nil {
		_ = os.Remove(name)
		return "", nil, fmt.Errorf("chmod embedded relay: %w", cerr)
	}
	return name, func() { _ = os.Remove(name) }, nil
}

// The cooperative-plane exec-stdio transport, HOST half (#2).
//
// Apple `container` has no host→container unix-socket forward (chafe C29) and the
// confined default is `--network none`, so an in-container cooperative adapter has
// no path to the daemon. This pump tunnels the daemon socket into the container
// over the STDIO of a SECOND `container exec` of the static relay binary
// (cmd/mad-substrate-relay). Each adapter connection the relay announces (OPEN) gets
// its OWN fresh daemon connection, so the adapter's own token-authed
// session.attach (MAD_SESSION_TOKEN, already injected) rebinds that
// connection to the launcher's session (Inv 4; multi-connection-per-session is
// allowed — RebindSession just sets cc.Session, leases are session-keyed). The
// pump NEVER interprets the bytes it carries; the daemon Authenticator is
// unchanged (it only ever sees same-uid unix connections from this launcher).
//
// DEADLOCK-FREE BY CONSTRUCTION (review finding): the single demux loop NEVER
// performs a blocking write, and no lock is ever held across a blocking pipe/conn
// write. Outbound frames go through ONE dedicated writer goroutine fed by a
// buffered channel; each stream owns a bounded inbound queue drained by its own
// writer goroutine; the demux loop only does non-blocking map ops + non-blocking
// enqueues, and SHEDS a stream whose queue overflows (a stuck/slow stream is
// sacrificed, never the whole multiplex). The daemon dial happens OFF the demux
// loop (in the per-stream goroutine), so a slow connect cannot stall other streams.
//
// FAIL-SOFT: the cooperative plane is a COORDINATION nicety, not a safety
// boundary — the container hard floor (FS confinement + integrator gate) already
// confines the agent. A relay that will not start must NEVER block the agent; the
// caller logs and runs the agent confined without the plane.

const (
	// relayStageName is the staged relay's filename inside the writable scratch dir
	// (which is bind-mounted at the SAME host path in the container, so the in-
	// container exec path equals the host stage path).
	relayStageName = ".mad-substrate-relay"

	// madSubstrateStageName is the staged in-container mad-substrate binary's filename inside
	// the writable scratch dir (bind-mounted at the SAME host path in the container, so
	// the in-container path equals the host stage path). The agent's MCP config points
	// its `mad-substrate mcp` command at this path so a real claude/codex inside the guest
	// runs the staged binary, which dials MAD_SOCKET (the relay) under the
	// session token. THIS binary is darwin and absent from the guest image — hence the
	// staged linux binary from the embedded payload (coopembed).
	madSubstrateStageName = ".mad-substrate"

	// coopReadyTimeout bounds how long startCoop waits for the relay to signal its
	// listener is bound before giving up (fail-soft).
	coopReadyTimeout = 5 * time.Second
	// coopShutdownTimeout bounds graceful relay-exec teardown before a Kill backstop
	// (the boundary teardown's `container rm -f` is the ultimate reaper).
	coopShutdownTimeout = 3 * time.Second
	// coopStreamQueue bounds a single stream's in-flight inbound frames. A stream
	// that backs up past this is SHED (closed) so it can never wedge the multiplex;
	// generous so a merely-bursty healthy stream is never falsely closed.
	coopStreamQueue = 256
	// coopOutQueue bounds the shared outbound frame channel feeding the writer.
	coopOutQueue = 256
	// coopDialTimeout bounds the per-stream daemon dial (a local unix connect is
	// near-instant; this is the safety bound).
	coopDialTimeout = 3 * time.Second
)

// coopConfig parameterizes one cooperative-plane tunnel.
type coopConfig struct {
	containerBin    string // the `container` CLI (package var containerBin in production)
	containerID     string // the confined container to exec the relay into
	relayHostPath   string // host path of the static linux relay binary
	scratchDir      string // writable dir mounted at the SAME path in-container (MAD_SCRATCH)
	inContainerSock string // unix socket the relay listens on (also the agent's MAD_SOCKET)
	daemonSocket    string // host daemon socket the pump dials per stream
	logf            func(format string, args ...any)
}

// startCoop stages the relay, starts it as a second `container exec` whose stdio
// is the tunnel, and runs the demux pump. It returns once the relay has signaled
// readiness (so the agent's adapter will find the socket bound), plus a stop func
// the caller defers. ANY failure returns an error and NO tunnel — the caller
// treats that as fail-soft (run the agent confined without the plane).
func startCoop(cfg coopConfig) (stop func(), err error) {
	if cfg.logf == nil {
		cfg.logf = func(string, ...any) {}
	}
	inCtrRelay, err := stageRelay(cfg.relayHostPath, cfg.scratchDir)
	if err != nil {
		return nil, fmt.Errorf("stage relay: %w", err)
	}

	// A second exec into the SAME container; -i keeps stdin open as the tunnel.
	// NOT -t: a PTY would impose line-discipline translation and corrupt the binary
	// frame stream (the feasibility probe used -i only, no -t).
	cmd := exec.Command(cfg.containerBin, "exec", "-i", cfg.containerID, inCtrRelay, cfg.inContainerSock)
	cmd.Env = os.Environ() // inherit PATH so the `container` CLI resolves (mirror buildContainerExec)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = &lineLogWriter{logf: cfg.logf, prefix: "coop relay"}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay exec: %w", err)
	}

	daemonSocket := cfg.daemonSocket
	p := newCoopPump(stdin, stdout, func() (net.Conn, error) {
		return net.DialTimeout("unix", daemonSocket, coopDialTimeout)
	}, cfg.logf)
	go p.run()

	select {
	case <-p.ready:
		cfg.logf("coop: relay ready; tunneling daemon %s into container %s via %s", daemonSocket, cfg.containerID, cfg.inContainerSock)
	case <-p.exited:
		// The relay died before binding its listener (wrong arch, listen failure,
		// crash). Fail over to confined mode IMMEDIATELY rather than after the full
		// readiness timeout.
		reap(cmd, coopShutdownTimeout)
		return nil, fmt.Errorf("relay exited before signaling ready")
	case <-time.After(coopReadyTimeout):
		p.shutdown()
		_ = stdin.Close()
		p.waitExited(coopShutdownTimeout) // join run() before reap (StdoutPipe-vs-Wait)
		reap(cmd, coopShutdownTimeout)
		return nil, fmt.Errorf("relay did not signal ready within %s", coopReadyTimeout)
	}

	return func() {
		p.shutdown()      // unblock writer + all stream goroutines
		_ = stdin.Close() // relay sees EOF → exits → its stdout closes → run() ends
		p.waitExited(coopShutdownTimeout)
		reap(cmd, coopShutdownTimeout)
	}, nil
}

// stageRelay copies the host relay binary into the agent's writable scratch dir
// (which is bind-mounted at the SAME host path inside the container) and returns
// that path — which is therefore ALSO the in-container exec path. Writes to a temp
// then renames so a concurrent exec can never observe a half-written binary.
func stageRelay(hostPath, scratchDir string) (inContainerPath string, err error) {
	if strings.TrimSpace(hostPath) == "" {
		return "", fmt.Errorf("no relay binary configured")
	}
	if strings.TrimSpace(scratchDir) == "" {
		return "", fmt.Errorf("no scratch dir to stage the relay into")
	}
	src, err := os.Open(hostPath)
	if err != nil {
		return "", err
	}
	defer src.Close()
	dstPath := filepath.Join(scratchDir, relayStageName)
	tmp := dstPath + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dstPath); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return dstPath, nil
}

// stageMadSubstrate writes the embedded static linux mad-substrate binary for goarch into the
// agent's writable scratch dir at madSubstrateStageName (0755, atomic temp+rename) and
// returns that path — which is ALSO the in-container path (the scratch is bind-mounted
// at its identical host path in the container). It is the mad-substrate-binary analogue of
// stageRelay, except the source is the EMBEDDED payload (coopembed) rather than a host
// file, since THIS launcher's own binary is darwin and absent from the guest image.
// Returns an error when no payload is embedded for goarch (the untagged build) — the
// caller treats that as fail-soft (the agent runs confined without in-container MCP
// wiring).
func stageMadSubstrate(scratchDir, goarch string) (inContainerPath string, err error) {
	b, ok := madSubstrateBytesFn(goarch)
	if !ok {
		return "", fmt.Errorf("no embedded linux mad-substrate binary for %s", goarch)
	}
	return stageBytes(b, scratchDir, madSubstrateStageName)
}

// stageBytes writes b into scratchDir/name (0755), atomically via a temp file + rename
// so a concurrent exec can never observe a half-written binary, and returns the path —
// which equals the in-container path (the scratch is bind-mounted at its identical host
// path). Shared by the byte-sourced stage (the embedded mad-substrate binary); stageRelay
// stays file-sourced because its override may point at an arbitrary host file.
func stageBytes(b []byte, scratchDir, name string) (string, error) {
	if strings.TrimSpace(scratchDir) == "" {
		return "", fmt.Errorf("no scratch dir to stage into")
	}
	dstPath := filepath.Join(scratchDir, name)
	tmp := dstPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return "", err
	}
	// WriteFile's mode is masked by the launcher's umask; force 0755 so the staged
	// binary is ALWAYS executable in-container regardless of that umask.
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dstPath); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return dstPath, nil
}

// reap closes out the relay exec process: wait for exit, Kill backstop on timeout.
// The boundary teardown's `container rm -f` is the ultimate reaper of the in-
// container relay regardless.
func reap(cmd *exec.Cmd, timeout time.Duration) {
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
}

// coopPump demultiplexes relay→pump frames onto per-stream daemon connections and
// multiplexes daemon bytes back. It is decoupled from the container so it is
// testable with in-memory pipes and a fake daemon dialer.
type coopPump struct {
	in   io.Writer                // pump→relay (the relay's stdin)
	out  io.Reader                // relay→pump (the relay's stdout)
	dial func() (net.Conn, error) // dials a FRESH daemon connection per stream
	logf func(format string, a ...any)

	ready     chan struct{}
	readyOnce sync.Once
	exited    chan struct{} // closed when run() returns (fast fail-soft + teardown join)
	exitOnce  sync.Once
	done      chan struct{} // closed once on shutdown to unblock every goroutine
	doneOnce  sync.Once
	outCh     chan []byte // shared outbound frames; the SOLE writer goroutine drains it

	mu      sync.Mutex
	streams map[uint32]*pumpStream
	closed  bool
}

// pumpStream is one multiplexed adapter↔daemon connection.
type pumpStream struct {
	id            uint32
	toDaemon      chan []byte // demux loop enqueues (non-blocking); the stream writer drains to dc
	closed        chan struct{}
	once          sync.Once
	peerInitiated atomic.Bool // the relay already closed its side → don't echo a CLOSE back
	mu            sync.Mutex
	dc            net.Conn
}

func (s *pumpStream) setConn(dc net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
		return false // torn down before the dial completed
	default:
		s.dc = dc
		return true
	}
}

func (s *pumpStream) teardown() {
	s.once.Do(func() {
		close(s.closed)
		s.mu.Lock()
		dc := s.dc
		s.mu.Unlock()
		if dc != nil {
			_ = dc.Close()
		}
	})
}

func newCoopPump(in io.Writer, out io.Reader, dial func() (net.Conn, error), logf func(string, ...any)) *coopPump {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &coopPump{
		in:      in,
		out:     out,
		dial:    dial,
		logf:    logf,
		ready:   make(chan struct{}),
		exited:  make(chan struct{}),
		done:    make(chan struct{}),
		outCh:   make(chan []byte, coopOutQueue),
		streams: map[uint32]*pumpStream{},
	}
}

// run reads the relay frame stream and dispatches; it NEVER blocks on I/O. It owns
// the lifecycle of the writer goroutine and tears everything down on exit.
func (p *coopPump) run() {
	go p.writeLoop()
	defer func() {
		p.exitOnce.Do(func() { close(p.exited) })
		p.shutdown()
	}()
	for {
		f, err := coop.ReadFrame(p.out)
		if err != nil {
			return
		}
		switch f.Type {
		case coop.FrameReady:
			p.readyOnce.Do(func() { close(p.ready) })
		case coop.FrameOpen:
			p.openStream(f.StreamID)
		case coop.FrameData:
			p.dispatchData(f.StreamID, f.Payload)
		case coop.FrameClose:
			p.peerClose(f.StreamID)
		}
	}
}

// writeLoop is the SOLE writer to p.in: it drains outCh, so no producer ever holds
// a lock across the blocking pipe write. If the pipe write FAILS (the relay's read
// half broke), it triggers a full shutdown so every enqueue() producer wakes via
// <-done instead of stranding on a channel with no drainer.
func (p *coopPump) writeLoop() {
	defer p.shutdown()
	for {
		select {
		case b := <-p.outCh:
			if _, err := p.in.Write(b); err != nil {
				return
			}
		case <-p.done:
			return
		}
	}
}

// enqueue serializes one frame onto outCh; it blocks only until the writer drains
// or shutdown. Returns false if shutting down. Callers are per-stream goroutines
// (NEVER the demux loop), so blocking here cannot stall the demux.
func (p *coopPump) enqueue(id uint32, t coop.FrameType, payload []byte) bool {
	b, err := coop.EncodeFrame(id, t, payload)
	if err != nil {
		return false
	}
	select {
	case p.outCh <- b:
		return true
	case <-p.done:
		return false
	}
}

// enqueueStream is enqueue for a per-stream producer (the daemon reader): it ALSO
// unblocks when THIS stream is torn down, so a reader parked on a full outCh exits
// promptly on its own stream's teardown rather than lingering until shutdown.
func (p *coopPump) enqueueStream(s *pumpStream, t coop.FrameType, payload []byte) bool {
	b, err := coop.EncodeFrame(s.id, t, payload)
	if err != nil {
		return false
	}
	select {
	case p.outCh <- b:
		return true
	case <-s.closed:
		return false
	case <-p.done:
		return false
	}
}

// openStream registers a new stream and starts its goroutine. NON-BLOCKING (the
// dial happens inside serveStream).
func (p *coopPump) openStream(id uint32) {
	s := &pumpStream{id: id, toDaemon: make(chan []byte, coopStreamQueue), closed: make(chan struct{})}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if _, exists := p.streams[id]; exists {
		p.mu.Unlock()
		p.logf("coop: duplicate OPEN for live stream %d; ignoring", id)
		return
	}
	p.streams[id] = s
	p.mu.Unlock()
	go p.serveStream(s)
}

// serveStream dials a fresh daemon connection for the stream, then runs a reader
// (daemon→outCh) and a writer (toDaemon→daemon) until either side ends.
func (p *coopPump) serveStream(s *pumpStream) {
	dc, err := p.dial()
	if err != nil {
		p.logf("coop: daemon dial for stream %d failed: %v", s.id, err)
		p.removeStream(s.id)
		p.enqueue(s.id, coop.FrameClose, nil) // tell the relay to drop the adapter conn
		return
	}
	if !s.setConn(dc) {
		// The stream was torn down DURING the dial (a CLOSE from the relay, an
		// overflow SHED, or shutdown). The deferred cleanup below is not registered
		// yet, so do it here: drop the map entry, and — unless the relay already
		// closed its side — tell it to drop the adapter conn (else it would hang).
		_ = dc.Close()
		p.removeStream(s.id)
		if !s.peerInitiated.Load() {
			p.enqueue(s.id, coop.FrameClose, nil)
		}
		return
	}
	defer func() {
		s.teardown() // close conn + signal (idempotent)
		p.removeStream(s.id)
		// Tell the relay to drop the adapter conn — UNLESS the relay is the one that
		// closed (we received a CLOSE), in which case echoing back is redundant.
		if !s.peerInitiated.Load() {
			p.enqueue(s.id, coop.FrameClose, nil)
		}
	}()

	go func() { // reader: daemon → outCh
		buf := make([]byte, 32*1024)
		for {
			n, e := dc.Read(buf)
			if n > 0 {
				if !p.enqueueStream(s, coop.FrameData, append([]byte(nil), buf[:n]...)) {
					break
				}
			}
			if e != nil {
				break
			}
		}
		s.teardown() // wake the writer
	}()

	for { // writer: toDaemon → daemon
		select {
		case b := <-s.toDaemon:
			if _, e := dc.Write(b); e != nil {
				return
			}
		case <-s.closed:
			return
		case <-p.done:
			return
		}
	}
}

// dispatchData hands adapter bytes to a stream's bounded queue WITHOUT blocking
// the demux loop. If the queue is full (a stuck/slow daemon conn) the stream is
// SHED — sacrificed so it can never wedge the multiplex.
func (p *coopPump) dispatchData(id uint32, payload []byte) {
	p.mu.Lock()
	s := p.streams[id]
	p.mu.Unlock()
	if s == nil {
		return // unknown/closed stream — drop
	}
	select {
	case s.toDaemon <- payload:
	case <-s.closed:
	default:
		p.logf("coop: stream %d outbound queue full; shedding the slow stream", id)
		s.teardown() // peerInitiated stays false → serveStream tells the relay to drop it
	}
}

// peerClose tears down a stream after RECEIVING a CLOSE from the relay; it marks
// the stream peer-initiated so serveStream does not echo a redundant CLOSE back.
// Non-blocking. (Overflow shedding calls teardown directly so the CLOSE IS sent.)
func (p *coopPump) peerClose(id uint32) {
	p.mu.Lock()
	s := p.streams[id]
	p.mu.Unlock()
	if s != nil {
		s.peerInitiated.Store(true)
		s.teardown() // wakes the writer (select) AND a writer stuck in dc.Write (via dc.Close)
	}
}

func (p *coopPump) removeStream(id uint32) {
	p.mu.Lock()
	delete(p.streams, id)
	p.mu.Unlock()
}

// shutdown unblocks the writer + every stream goroutine (idempotent).
func (p *coopPump) shutdown() {
	p.doneOnce.Do(func() { close(p.done) })
	p.mu.Lock()
	p.closed = true
	streams := make([]*pumpStream, 0, len(p.streams))
	for _, s := range p.streams {
		streams = append(streams, s)
	}
	p.mu.Unlock()
	for _, s := range streams {
		s.teardown()
	}
}

func (p *coopPump) waitExited(d time.Duration) bool {
	select {
	case <-p.exited:
		return true
	case <-time.After(d):
		return false
	}
}

// lineLogWriter routes the relay exec's stderr to the launcher's diagnostics sink.
type lineLogWriter struct {
	logf   func(format string, a ...any)
	prefix string
}

func (w *lineLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		w.logf("%s: %s", w.prefix, msg)
	}
	return len(p), nil
}
