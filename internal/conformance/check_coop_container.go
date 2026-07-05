package conformance

import (
	"encoding/binary"
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
)

// check_coop_container.go is the COOPERATIVE-PLANE container row: it proves the
// Inv-4 "shared session authority" property end-to-end for an IN-CONTAINER client.
// Under the container grain Apple `container` has NO host→container unix-socket
// forward and the agent has no host path to the daemon socket, so a cooperative
// client inside the container reaches the arbiter ONLY through the relay tunnel
// (cmd/mad-trellis-relay exec'd into the container, its stdio pumped to the daemon by
// the host half). The client then presents the launcher's daemon-minted capability
// token via session.attach, which REBINDS its forwarded connection to the
// launcher's session — so a lease it takes INSIDE the container is held by the
// LAUNCHER's session id (Inv 4: the cooperative adapter acts under the SHARED
// identity, never a fresh per-connection one). This is the cooperative-plane analogue
// of the structural confinement tier — same black-box, runtime-gated discipline.
//
// NON-VACUOUS CONTROL (the cardinal rule): the authority is REAL only if a FORGED
// token cannot obtain it. The control drives the SAME tunnel twice — a VALID token
// (which MUST attach to the launcher session, proving the predicate + tunnel are
// live) and a FORGED token (which MUST be REJECTED by session.attach, so the
// in-container client gets NO authority and never binds the launcher session). The
// predicate "attaches to the launcher session" thus DISTINGUISHES the valid token
// from the forged one; were attach a no-op that accepted anything, the forged token
// would bind the launcher session and the control flips the row RED.
//
// SKIP-WITH-REASON. The row is RUNTIME-GATED: with no `container` runtime (a
// runtime-less CI host) it returns a SKIPPED Result naming the reason, so the gate
// stays GREEN. It additionally needs the static linux relay + probe binaries staged
// into the container (this darwin/host binary cannot run inside a linux guest); their
// host paths are supplied via MAD_COOP_LIVE_RELAY / MAD_COOP_LIVE_PROBE
// (the same knobs the launcher live e2e uses). When either is absent the row SKIPs
// with that reason rather than asserting vacuously.
//
// BLACK BOX. Everything speaks the PUBLIC surface only: substrate.provision /
// teardown, session.whoami / mint_token, lease.acquire / list (over the wire client),
// the SAME out-of-band `container` CLI the confinement tier drives as the
// uncooperative agent, and an in-container client (the coopprobe) that itself speaks
// only the frozen public protocol. No forbidden internal package is imported
// (readonly_imports_test still passes) — in particular the coop frame codec + host
// pump are reimplemented here from stdlib, never imported from internal/coop or
// internal/launcher.

func init() {
	RegisterCheck(coopContainerAuthority{})
}

// coopOwner is the owning project for the cooperative-plane in-container tier.
const coopOwner = "cooperative-plane/in-container-adapter"

// coopInContainerSocket is the unix socket the relay binds INSIDE the container
// (also the in-container client's MAD_SOCKET). It lives on the writable
// in-container /tmp (never /work, so it cannot pollute the agent's git clone),
// mirroring the launcher's coopSocketPath. The relay + the probe both run inside the
// SAME container, so both resolve this path; it is NOT a host path.
const coopInContainerSocket = "/tmp/mad-trellis-coop.sock"

// coopRelayEnv / coopProbeEnv name the host paths of the prebuilt STATIC LINUX relay
// and probe binaries staged into the container. They are reused from the launcher's
// live e2e so an operator points them once. Absent → the row SKIPs (this host binary
// is darwin and cannot exec inside a linux guest, and the package may not import the
// embedded payload).
const (
	coopRelayEnv = "MAD_COOP_LIVE_RELAY"
	coopProbeEnv = "MAD_COOP_LIVE_PROBE"
)

type coopContainerAuthority struct{}

func (coopContainerAuthority) ID() string           { return "coop-container-authority" }
func (coopContainerAuthority) OwnerProject() string { return coopOwner }
func (coopContainerAuthority) Clause() string {
	return "0003 §9b / Inv 4 (cooperative plane, container grain): an IN-CONTAINER cooperative client reaches the daemon ONLY through the relay tunnel (no host socket path under the container grain) and, via its token-authed session.attach, acts under EXACTLY the LAUNCHER's session — a lease it takes inside the container is HELD BY THE LAUNCHER session id (shared identity, Inv 4), never a fresh per-connection one. A FORGED token is REJECTED so the in-container client gets NO authority"
}

func (c coopContainerAuthority) Run(s *Scratch) Result {
	if ok, reason := ContainerRuntimeAvailable(); !ok {
		return skip(c, "coop-container-authority: %s", reason)
	}
	relayBin, probeBin, ok, reason := coopBinaries()
	if !ok {
		return skip(c, "coop-container-authority: %s", reason)
	}
	// The cooperative plane pairs with the COOPERATIVE container grain (writable rootfs +
	// a writable /tmp for the relay's socket); confinement is a separate tier. Boot the
	// daemon on the (non-confined) container grain.
	if err := s.UseContainerGrain(); err != nil {
		return fail(c, "boot daemon on the container grain: %v", err)
	}

	cb, err := s.ProvisionContainer()
	if err != nil {
		return fail(c, "provision container boundary: %v", err)
	}
	defer cb.Teardown()

	sess, scratch, token, ferr := s.coopEstablishSession(cb)
	if ferr != nil {
		return fail(c, "%v", ferr)
	}

	stop, err := s.startCoopTunnel(cb.ContainerID, scratch, relayBin)
	if err != nil {
		return fail(c, "bring up the relay tunnel into the container: %v", err)
	}
	defer stop()

	// The in-container client attaches with the LAUNCHER's token, so it MUST bind the
	// launcher session, and a lease it takes inside MUST be held by the launcher session.
	out, perr := runCoopProbe(cb.ContainerID, scratch, probeBin, coopInContainerSocket, token, sess)
	if perr != nil {
		return fail(c, "in-container client (valid token) failed: %v\n%s", perr, out)
	}
	if !strings.Contains(out, "COOP-PROBE-OK") {
		return fail(c, "in-container client did not report success:\n%s", out)
	}
	if !strings.Contains(out, "attached session="+sess) {
		return fail(c, "BREACH: the in-container client did not attach to the LAUNCHER session %q (shared identity not established):\n%s", sess, out)
	}
	if !strings.Contains(out, "holder="+sess) {
		return fail(c, "BREACH: a lease taken INSIDE the container is NOT held by the launcher session %q — the cooperative client did not act under the shared identity:\n%s", sess, out)
	}

	return pass(c, "cooperative plane (container grain): an in-container client reached the daemon through the relay tunnel (no host socket path), attached to the LAUNCHER session %q via its capability token, and a lease it took INSIDE the container is HELD BY that session (Inv 4 shared identity)", sess)
}

func (c coopContainerAuthority) Control(s *Scratch) error {
	if ok, _ := ContainerRuntimeAvailable(); !ok {
		return nil // nothing to assert non-vacuously on a runtime-less host
	}
	relayBin, probeBin, ok, _ := coopBinaries()
	if !ok {
		return nil // the relay/probe binaries are not available to stage — cannot drive the tunnel
	}
	if err := s.UseContainerGrain(); err != nil {
		return fmt.Errorf("control boot container grain: %w", err)
	}

	cb, err := s.ProvisionContainer()
	if err != nil {
		return fmt.Errorf("control provision container boundary: %w", err)
	}
	defer cb.Teardown()

	sess, scratch, token, ferr := s.coopEstablishSession(cb)
	if ferr != nil {
		return fmt.Errorf("control establish session: %w", ferr)
	}

	stop, err := s.startCoopTunnel(cb.ContainerID, scratch, relayBin)
	if err != nil {
		return fmt.Errorf("control bring up relay tunnel: %w", err)
	}
	defer stop()

	// (1) Prove the predicate + tunnel are LIVE: a VALID token MUST attach to the
	// launcher session. If it does not, the forged-token rejection below would prove
	// nothing (the tunnel could simply be dead).
	validOut, validErr := runCoopProbe(cb.ContainerID, scratch, probeBin, coopInContainerSocket, token, "")
	if validErr != nil || !strings.Contains(validOut, "attached session="+sess) {
		return fmt.Errorf("CONTROL VACUOUS: a VALID token did NOT attach to the launcher session %q through the tunnel (err=%v) — the forged-token rejection cannot be trusted:\n%s", sess, validErr, validOut)
	}

	// (2) INJECT the violation: a FORGED token must be REJECTED by session.attach, so the
	// in-container client gets NO authority and never binds the launcher session. If a
	// forged token DID bind the launcher session, that is an impersonation breach and the
	// Run's authority assertion is vacuous — flip RED.
	forgedOut, forgedErr := runCoopProbe(cb.ContainerID, scratch, probeBin, coopInContainerSocket, coopForgedToken, "")
	if forgedErr == nil {
		return fmt.Errorf("CONTROL: a FORGED token was ACCEPTED by session.attach (the in-container client succeeded) — impersonation is possible:\n%s", forgedOut)
	}
	if strings.Contains(forgedOut, "attached session="+sess) {
		return fmt.Errorf("CONTROL: a FORGED token BOUND the launcher session %q — impersonation breach; the authority predicate does not distinguish a valid token from a forged one:\n%s", sess, forgedOut)
	}
	if strings.Contains(forgedOut, "COOP-PROBE-OK") {
		return fmt.Errorf("CONTROL: a FORGED token obtained authority (COOP-PROBE-OK) — it should have been rejected at attach:\n%s", forgedOut)
	}
	return nil
}

// coopForgedToken is a deliberately INVALID capability token: it was never minted by
// the daemon, so its session.attach MUST be rejected (the control's injected
// violation). Its exact value is irrelevant — only that it is absent from the
// daemon's durable token store.
const coopForgedToken = "nm-forged-capability-token-NOT-MINTED-by-the-daemon"

// coopBinaries resolves the host paths of the prebuilt static linux relay + probe
// binaries to stage into the container, returning ok=false + a reason when either is
// unset (the row then SKIPs). They must exist + be regular files.
func coopBinaries() (relay, probe string, ok bool, reason string) {
	relay = strings.TrimSpace(os.Getenv(coopRelayEnv))
	probe = strings.TrimSpace(os.Getenv(coopProbeEnv))
	if relay == "" || probe == "" {
		return "", "", false, fmt.Sprintf("set %s + %s to the static linux relay/probe binaries (this host binary cannot run inside a linux guest)", coopRelayEnv, coopProbeEnv)
	}
	for _, p := range []string{relay, probe} {
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			return "", "", false, fmt.Sprintf("relay/probe binary %q is not a usable file: %v", p, err)
		}
	}
	return relay, probe, true, ""
}

// coopEstablishSession learns the held connection's daemon-minted session id, mints
// the SHAREABLE capability token, and acquires the session-liveness lease (long TTL)
// so the in-container client's token-authed attach passes the daemon's liveness gate.
// Everything runs on the HELD provisioning connection (cb.conn) so the minted token
// is bound to the SAME session that owns the boundary (Inv 4). Also returns the
// boundary's MAD_SCRATCH dir (bind-mounted at the same path in-container — the
// stage target for the relay + probe).
func (s *Scratch) coopEstablishSession(cb *ContainerBoundary) (sess, scratch, token string, err error) {
	sess, err = whoAmIOn(cb.conn)
	if err != nil {
		return "", "", "", fmt.Errorf("whoami on the held connection: %w", err)
	}
	scratch = cb.Env["MAD_SCRATCH"]
	if scratch == "" {
		return "", "", "", fmt.Errorf("no MAD_SCRATCH in the provisioned boundary env (cannot stage the relay/probe)")
	}
	var mt struct {
		Token       string `json:"token"`
		LivenessKey string `json:"liveness_key"`
	}
	if cerr := cb.conn.Call("session.mint_token", map[string]any{}, &mt); cerr != nil {
		return "", "", "", fmt.Errorf("mint capability token: %w", cerr)
	}
	if mt.Token == "" || mt.LivenessKey == "" {
		return "", "", "", fmt.Errorf("daemon returned an empty token or liveness key")
	}
	var acq struct {
		Granted bool `json:"granted"`
	}
	if cerr := cb.conn.Call("lease.acquire", map[string]any{"key": mt.LivenessKey, "ttl_ms": 120000}, &acq); cerr != nil {
		return "", "", "", fmt.Errorf("acquire session-liveness lease: %w", cerr)
	}
	if !acq.Granted {
		return "", "", "", fmt.Errorf("session-liveness lease was not granted")
	}
	return sess, scratch, mt.Token, nil
}

// runCoopProbe stages the in-container client (coopprobe) into the boundary's
// writable scratch (bind-mounted at the same path in-container) and execs it INSIDE
// the container — exactly the launcher's container-exec path, driven directly. It
// connects to the relay socket, attaches with `token` (when non-empty), and asserts
// against `expect` (when non-empty). Returns the combined output + a non-nil error
// iff the in-container command exited non-zero (a rejected attach exits non-zero).
func runCoopProbe(containerID, scratchDir, probeHostPath, sock, token, expect string) (string, error) {
	staged, err := stageCoopBinary(probeHostPath, filepath.Join(scratchDir, ".mad-trellis-coopprobe"))
	if err != nil {
		return "", fmt.Errorf("stage probe: %w", err)
	}
	args := []string{"exec", containerID, staged, sock}
	if token != "" {
		args = append(args, token)
		if expect != "" {
			args = append(args, expect)
		}
	}
	cmd := exec.Command(containerBin, args...)
	cmd.Env = os.Environ() // inherit PATH so the `container` CLI resolves
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// stageCoopBinary copies a host binary into the agent's writable scratch dir (which
// is bind-mounted at the SAME host path inside the container) at dstPath, 0755, via a
// temp + rename so a concurrent exec never observes a half-written binary. dstPath is
// therefore ALSO the in-container exec path. Mirrors the launcher's stageRelay.
func stageCoopBinary(hostPath, dstPath string) (string, error) {
	in, err := os.Open(hostPath)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp := dstPath + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
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

// startCoopTunnel brings up the HOST half of the cooperative-plane exec-stdio
// transport: it stages the relay into the boundary's scratch, runs it as a SECOND
// `container exec -i` (stdio = the tunnel), and pumps the daemon socket into the
// container over that stdio. It returns once the relay signals its in-container
// listener is bound, plus a stop func the caller defers. ANY failure returns an error
// and NO tunnel. This reimplements the launcher's startCoop with a stdlib-only frame
// codec + pump because the conformance package may NOT import internal/launcher or
// internal/coop.
func (s *Scratch) startCoopTunnel(containerID, scratchDir, relayHostPath string) (stop func(), err error) {
	staged, err := stageCoopBinary(relayHostPath, filepath.Join(scratchDir, ".mad-trellis-relay"))
	if err != nil {
		return nil, fmt.Errorf("stage relay: %w", err)
	}
	// -i keeps stdin open as the tunnel; NOT -t (a PTY would corrupt the binary frame
	// stream via line-discipline translation).
	cmd := exec.Command(containerBin, "exec", "-i", containerID, staged, coopInContainerSocket)
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay exec: %w", err)
	}

	sock := s.Socket
	p := newCoopHostPump(stdin, stdout, func() (net.Conn, error) {
		return net.DialTimeout("unix", sock, 3*time.Second)
	})
	go p.run()

	select {
	case <-p.ready:
	case <-p.exited:
		reapCoop(cmd)
		return nil, fmt.Errorf("relay exited before signaling ready")
	case <-time.After(5 * time.Second):
		p.shutdown()
		_ = stdin.Close()
		p.waitExited(3 * time.Second)
		reapCoop(cmd)
		return nil, fmt.Errorf("relay did not signal ready within 5s")
	}

	return func() {
		p.shutdown()
		_ = stdin.Close()
		p.waitExited(3 * time.Second)
		reapCoop(cmd)
	}, nil
}

// reapCoop closes out the relay exec process (wait, Kill backstop on timeout). The
// boundary teardown's `container rm -f` is the ultimate reaper regardless.
func reapCoop(cmd *exec.Cmd) {
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
}

// ----------------------------------------------------------------------------
// coop frame codec — a stdlib-only reimplementation of internal/coop's framing
// (the conformance package may not import internal/coop). header = streamID(4) +
// type(1) + length(4), big-endian; OPEN/DATA/CLOSE/READY as in the relay.
// ----------------------------------------------------------------------------

const (
	coopFrameOpen  byte = 1
	coopFrameData  byte = 2
	coopFrameClose byte = 3
	coopFrameReady byte = 4
	coopHeaderLen       = 9
	coopMaxPayload      = 1 << 20
)

func coopEncode(id uint32, t byte, payload []byte) []byte {
	buf := make([]byte, coopHeaderLen+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], id)
	buf[4] = t
	binary.BigEndian.PutUint32(buf[5:9], uint32(len(payload)))
	copy(buf[coopHeaderLen:], payload)
	return buf
}

func coopReadFrame(r io.Reader) (id uint32, t byte, payload []byte, err error) {
	var hdr [coopHeaderLen]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, 0, nil, err
	}
	id = binary.BigEndian.Uint32(hdr[0:4])
	t = hdr[4]
	n := binary.BigEndian.Uint32(hdr[5:9])
	if n > coopMaxPayload {
		return 0, 0, nil, fmt.Errorf("coop: frame length %d exceeds max %d", n, coopMaxPayload)
	}
	if n > 0 {
		payload = make([]byte, n)
		if _, err = io.ReadFull(r, payload); err != nil {
			return 0, 0, nil, err
		}
	}
	return id, t, payload, nil
}

// ----------------------------------------------------------------------------
// coopHostPump — the host half of the tunnel: demultiplex relay→pump frames onto
// per-stream daemon connections and multiplex daemon bytes back. Deadlock-free by
// construction (mirrors internal/launcher/coop.go): one writer goroutine drains a
// buffered outbound channel; each stream owns a bounded inbound queue + its own
// goroutines; the demux loop only does non-blocking map ops + non-blocking enqueues
// and SHEDS an overflowing stream. The daemon dial happens OFF the demux loop.
// ----------------------------------------------------------------------------

const (
	coopStreamQueue = 256
	coopOutQueue    = 256
)

type coopHostPump struct {
	in   io.Writer
	out  io.Reader
	dial func() (net.Conn, error)

	ready     chan struct{}
	readyOnce sync.Once
	exited    chan struct{}
	exitOnce  sync.Once
	done      chan struct{}
	doneOnce  sync.Once
	outCh     chan []byte

	mu      sync.Mutex
	streams map[uint32]*coopHostStream
	closed  bool
}

type coopHostStream struct {
	id            uint32
	toDaemon      chan []byte
	closed        chan struct{}
	once          sync.Once
	peerInitiated atomic.Bool
	mu            sync.Mutex
	dc            net.Conn
}

func (s *coopHostStream) setConn(dc net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
		return false
	default:
		s.dc = dc
		return true
	}
}

func (s *coopHostStream) teardown() {
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

func newCoopHostPump(in io.Writer, out io.Reader, dial func() (net.Conn, error)) *coopHostPump {
	return &coopHostPump{
		in:      in,
		out:     out,
		dial:    dial,
		ready:   make(chan struct{}),
		exited:  make(chan struct{}),
		done:    make(chan struct{}),
		outCh:   make(chan []byte, coopOutQueue),
		streams: map[uint32]*coopHostStream{},
	}
}

func (p *coopHostPump) run() {
	go p.writeLoop()
	defer func() {
		p.exitOnce.Do(func() { close(p.exited) })
		p.shutdown()
	}()
	for {
		id, t, payload, err := coopReadFrame(p.out)
		if err != nil {
			return
		}
		switch t {
		case coopFrameReady:
			p.readyOnce.Do(func() { close(p.ready) })
		case coopFrameOpen:
			p.openStream(id)
		case coopFrameData:
			p.dispatchData(id, payload)
		case coopFrameClose:
			p.peerClose(id)
		}
	}
}

func (p *coopHostPump) writeLoop() {
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

func (p *coopHostPump) enqueue(id uint32, t byte, payload []byte) bool {
	b := coopEncode(id, t, payload)
	select {
	case p.outCh <- b:
		return true
	case <-p.done:
		return false
	}
}

func (p *coopHostPump) enqueueStream(s *coopHostStream, t byte, payload []byte) bool {
	b := coopEncode(s.id, t, payload)
	select {
	case p.outCh <- b:
		return true
	case <-s.closed:
		return false
	case <-p.done:
		return false
	}
}

func (p *coopHostPump) openStream(id uint32) {
	s := &coopHostStream{id: id, toDaemon: make(chan []byte, coopStreamQueue), closed: make(chan struct{})}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if _, exists := p.streams[id]; exists {
		p.mu.Unlock()
		return
	}
	p.streams[id] = s
	p.mu.Unlock()
	go p.serveStream(s)
}

func (p *coopHostPump) serveStream(s *coopHostStream) {
	dc, err := p.dial()
	if err != nil {
		p.removeStream(s.id)
		p.enqueue(s.id, coopFrameClose, nil)
		return
	}
	if !s.setConn(dc) {
		_ = dc.Close()
		p.removeStream(s.id)
		if !s.peerInitiated.Load() {
			p.enqueue(s.id, coopFrameClose, nil)
		}
		return
	}
	defer func() {
		s.teardown()
		p.removeStream(s.id)
		if !s.peerInitiated.Load() {
			p.enqueue(s.id, coopFrameClose, nil)
		}
	}()

	go func() { // reader: daemon → outCh
		buf := make([]byte, 32*1024)
		for {
			n, e := dc.Read(buf)
			if n > 0 {
				if !p.enqueueStream(s, coopFrameData, append([]byte(nil), buf[:n]...)) {
					break
				}
			}
			if e != nil {
				break
			}
		}
		s.teardown()
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

func (p *coopHostPump) dispatchData(id uint32, payload []byte) {
	p.mu.Lock()
	s := p.streams[id]
	p.mu.Unlock()
	if s == nil {
		return
	}
	select {
	case s.toDaemon <- payload:
	case <-s.closed:
	default:
		s.teardown()
	}
}

func (p *coopHostPump) peerClose(id uint32) {
	p.mu.Lock()
	s := p.streams[id]
	p.mu.Unlock()
	if s != nil {
		s.peerInitiated.Store(true)
		s.teardown()
	}
}

func (p *coopHostPump) removeStream(id uint32) {
	p.mu.Lock()
	delete(p.streams, id)
	p.mu.Unlock()
}

func (p *coopHostPump) shutdown() {
	p.doneOnce.Do(func() { close(p.done) })
	p.mu.Lock()
	p.closed = true
	streams := make([]*coopHostStream, 0, len(p.streams))
	for _, s := range p.streams {
		streams = append(streams, s)
	}
	p.mu.Unlock()
	for _, s := range streams {
		s.teardown()
	}
}

func (p *coopHostPump) waitExited(d time.Duration) bool {
	select {
	case <-p.exited:
		return true
	case <-time.After(d):
		return false
	}
}
