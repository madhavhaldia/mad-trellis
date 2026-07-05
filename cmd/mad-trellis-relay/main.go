// Command mad-trellis-relay is the in-container half of the cooperative-plane
// exec-stdio transport (#2). It is a tiny STATIC linux binary the launcher stages
// into the confined container and runs as a SECOND `container exec` whose stdio is
// the host↔guest tunnel.
//
// It listens on an in-container unix socket (argv[1], also the agent's
// MAD_SOCKET) that the cooperative adapter connects to, and MULTIPLEXES
// every adapter connection over its stdin/stdout to the host launcher pump, which
// forwards each to the daemon. It speaks ONLY the coop frame protocol and NEVER
// interprets the bytes it carries (the adapter performs its own token-authed
// session.attach end to end). It exits when stdin reaches EOF — the launcher
// closed the tunnel = teardown — or on an unrecoverable stdio error.
//
// DEADLOCK-FREE BY CONSTRUCTION (mirrors the pump): the stdin demux loop NEVER
// blocks on a conn write, and no lock is held across the blocking stdout write.
// Outbound frames funnel through ONE dedicated writer goroutine fed by a buffered
// channel; each adapter connection has a bounded inbound queue drained by its own
// writer goroutine; the demux loop only does non-blocking map ops + non-blocking
// enqueues and SHEDS a connection whose queue overflows (a stuck adapter is
// sacrificed, never the whole multiplex).
//
// Confinement: it binds the socket on the writable tmpfs /tmp inside the
// container (NOT /work, so it never pollutes the agent's git clone), runs under
// --cap-drop ALL / --read-only / --network none unchanged, and opens no network.
// cgo-free, stdlib-only — cross-compiled GOOS=linux GOARCH=arm64 CGO_ENABLED=0.
package main

import (
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/madhavhaldia/mad-trellis/internal/coop"
)

const (
	// relayConnQueue bounds a single adapter connection's in-flight inbound frames;
	// a connection that backs up past this is SHED so it can never wedge the mux.
	relayConnQueue = 256
	// relayOutQueue bounds the shared outbound frame channel feeding the writer.
	relayOutQueue = 256
)

func main() {
	log.SetPrefix("mad-trellis-relay: ")
	log.SetFlags(0)
	if len(os.Args) < 2 || os.Args[1] == "" {
		log.Fatal("usage: mad-trellis-relay <unix-socket-path>")
	}
	sockPath := os.Args[1]

	// Idempotent: drop a stale socket inode from a prior run.
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Fatalf("listen %s: %v", sockPath, err)
	}

	r := newRelay(os.Stdout)
	go r.writeLoop()

	// Signal readiness once the listener is bound, so the launcher knows the tunnel
	// is live before it execs the agent whose adapter will connect.
	if !r.enqueue(0, coop.FrameReady, nil) {
		_ = ln.Close()
		log.Fatal("write ready failed")
	}

	go r.accept(ln)

	// Drive the stdin demux loop on the main goroutine; it returns when the launcher
	// closes the tunnel (EOF) or the stream desyncs.
	r.readStdin(os.Stdin)

	// Teardown: stop accepting, unblock writers, drop every live adapter connection.
	_ = ln.Close()
	r.shutdown()
}

// relay multiplexes adapter unix connections over a single stdio pair.
type relay struct {
	out      io.Writer
	outCh    chan []byte
	done     chan struct{}
	doneOnce sync.Once

	mu      sync.Mutex
	streams map[uint32]*relayConn
	nextID  uint32
}

type relayConn struct {
	toAdapter     chan []byte
	closed        chan struct{}
	once          sync.Once
	peerInitiated atomic.Bool // the pump already closed its side → don't echo a CLOSE back
	conn          net.Conn
}

func newRelay(out io.Writer) *relay {
	return &relay{
		out:     out,
		outCh:   make(chan []byte, relayOutQueue),
		done:    make(chan struct{}),
		streams: map[uint32]*relayConn{},
	}
}

// writeLoop is the SOLE writer to stdout: it drains outCh so no producer holds a
// lock across the blocking stdout write. If the stdout write FAILS (the pump's read
// half broke), it triggers a full shutdown so every enqueue() producer wakes via
// <-done instead of stranding on a channel with no drainer.
func (r *relay) writeLoop() {
	defer r.shutdown()
	for {
		select {
		case b := <-r.outCh:
			if _, err := r.out.Write(b); err != nil {
				return
			}
		case <-r.done:
			return
		}
	}
}

// enqueue serializes one frame onto outCh; blocks only until the writer drains or
// shutdown. Callers are per-conn / accept goroutines (NEVER the demux loop).
func (r *relay) enqueue(id uint32, t coop.FrameType, payload []byte) bool {
	b, err := coop.EncodeFrame(id, t, payload)
	if err != nil {
		return false
	}
	select {
	case r.outCh <- b:
		return true
	case <-r.done:
		return false
	}
}

// enqueueStream is enqueue for a per-conn producer (the adapter reader): it ALSO
// unblocks when THIS connection is torn down, so a reader parked on a full outCh
// exits promptly on its own conn's teardown rather than lingering until shutdown.
func (r *relay) enqueueStream(id uint32, s *relayConn, t coop.FrameType, payload []byte) bool {
	b, err := coop.EncodeFrame(id, t, payload)
	if err != nil {
		return false
	}
	select {
	case r.outCh <- b:
		return true
	case <-s.closed:
		return false
	case <-r.done:
		return false
	}
}

// accept assigns a fresh stream id to each adapter connection, announces it with
// an OPEN frame, and serves it.
func (r *relay) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // listener closed (teardown)
		}
		s := &relayConn{toAdapter: make(chan []byte, relayConnQueue), closed: make(chan struct{}), conn: c}
		r.mu.Lock()
		select {
		case <-r.done:
			r.mu.Unlock()
			_ = c.Close()
			return
		default:
		}
		r.nextID++
		id := r.nextID
		r.streams[id] = s
		r.mu.Unlock()

		if !r.enqueue(id, coop.FrameOpen, nil) {
			r.tearDown(id)
			return
		}
		go r.serveConn(id, s)
	}
}

// serveConn runs a reader (adapter→outCh) and a writer (toAdapter→adapter) for one
// connection until either side ends.
func (r *relay) serveConn(id uint32, s *relayConn) {
	defer func() {
		r.tearDown(id)
		// Tell the pump to drop the daemon conn — UNLESS the pump is the one that
		// closed (we received a CLOSE), in which case echoing back is redundant.
		if !s.peerInitiated.Load() {
			r.enqueue(id, coop.FrameClose, nil)
		}
	}()

	go func() { // reader: adapter → outCh
		buf := make([]byte, 32*1024)
		for {
			n, e := s.conn.Read(buf)
			if n > 0 {
				if !r.enqueueStream(id, s, coop.FrameData, append([]byte(nil), buf[:n]...)) {
					break
				}
			}
			if e != nil {
				break
			}
		}
		r.signalClose(s) // wake the writer
	}()

	for { // writer: toAdapter → adapter
		select {
		case b := <-s.toAdapter:
			if _, e := s.conn.Write(b); e != nil {
				return
			}
		case <-s.closed:
			return
		case <-r.done:
			return
		}
	}
}

// readStdin demultiplexes pump→relay frames onto adapter connections WITHOUT ever
// blocking on a conn write.
func (r *relay) readStdin(in io.Reader) {
	for {
		f, err := coop.ReadFrame(in)
		if err != nil {
			return // EOF (tunnel closed) or desync — either way, stop.
		}
		switch f.Type {
		case coop.FrameData:
			r.dispatch(f.StreamID, f.Payload)
		case coop.FrameClose:
			r.peerClose(f.StreamID)
		case coop.FrameOpen, coop.FrameReady:
			// OPEN/READY only flow relay→pump; ignore if seen inbound (be lenient).
		}
	}
}

// dispatch hands pump bytes to a connection's bounded queue without blocking the
// demux loop; an overflowing (stuck) adapter is SHED.
func (r *relay) dispatch(id uint32, payload []byte) {
	r.mu.Lock()
	s := r.streams[id]
	r.mu.Unlock()
	if s == nil {
		return
	}
	select {
	case s.toAdapter <- payload:
	case <-s.closed:
	default:
		// Shed the slow adapter rather than wedge the mux. NOT peer-initiated, so
		// serveConn tells the pump to drop the daemon conn.
		r.signalClose(s)
	}
}

// peerClose tears down a connection after RECEIVING a CLOSE from the pump; it marks
// the connection peer-initiated so serveConn does not echo a redundant CLOSE back.
func (r *relay) peerClose(id uint32) {
	r.mu.Lock()
	s := r.streams[id]
	r.mu.Unlock()
	if s != nil {
		s.peerInitiated.Store(true)
		r.signalClose(s)
	}
}

// signalClose tears a connection down (idempotent): close the conn (waking a
// reader in Read AND a writer stuck in Write) and signal the writer loop.
func (r *relay) signalClose(s *relayConn) {
	s.once.Do(func() {
		close(s.closed)
		_ = s.conn.Close()
	})
}

func (r *relay) tearDown(id uint32) {
	r.mu.Lock()
	s := r.streams[id]
	if s != nil {
		delete(r.streams, id)
	}
	r.mu.Unlock()
	if s != nil {
		r.signalClose(s)
	}
}

// shutdown unblocks the writer + every connection goroutine (idempotent).
func (r *relay) shutdown() {
	r.doneOnce.Do(func() { close(r.done) })
	r.mu.Lock()
	conns := make([]*relayConn, 0, len(r.streams))
	for id, s := range r.streams {
		conns = append(conns, s)
		delete(r.streams, id)
	}
	r.mu.Unlock()
	for _, s := range conns {
		r.signalClose(s)
	}
}
