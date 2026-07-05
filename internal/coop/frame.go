// Package coop carries the cooperative-plane wire framing shared by the
// in-container relay (cmd/mad-trellis-relay) and the host launcher pump
// (internal/launcher/coop.go). It is the #2 "cooperative socket-into-container"
// transport: because Apple `container` has NO host→container unix-socket forward
// (chafe C29) and the confined default is `--network none`, the launcher tunnels
// the daemon socket into the container over the STDIO of a second `container
// exec` of the relay. Many adapter connections share that ONE stdio, so each is a
// logical STREAM identified by a uint32 and its lifecycle/bytes ride OPEN/DATA/
// CLOSE frames. READY is a one-shot relay→pump readiness signal sent once the
// relay's in-container listener is up (so the launcher knows the tunnel is live
// before it execs the agent whose adapter will connect).
//
// The codec is deliberately tiny and stdlib-only so the relay cross-compiles to a
// small static linux binary (cgo-free) and the same code links into the host
// launcher.
package coop

import (
	"encoding/binary"
	"fmt"
	"io"
)

// FrameType is the kind of a multiplexed frame.
type FrameType uint8

const (
	// FrameOpen announces a NEW stream — relay→pump ONLY (an adapter connected to
	// the in-container socket). The pump dials a fresh daemon connection for it, so
	// the adapter's own token-authed session.attach rebinds that connection to the
	// launcher's session.
	FrameOpen FrameType = 1
	// FrameData carries opaque payload bytes for a stream (BOTH directions). The
	// relay/pump never interpret these bytes.
	FrameData FrameType = 2
	// FrameClose ends a stream (BOTH directions): the originating side's connection
	// closed; the peer closes its paired connection. Idempotent on receipt.
	FrameClose FrameType = 3
	// FrameReady is a one-shot relay→pump signal (StreamID 0, no payload) emitted
	// once the relay's listener is bound — the launcher waits for it so the tunnel
	// is proven live before the agent (and its adapter) is exec'd.
	FrameReady FrameType = 4
)

// MaxPayload bounds a single frame's payload so a corrupt or hostile length
// prefix cannot drive an unbounded allocation on either side. Adapter RPC
// messages are small (KB); 1 MiB is generous headroom (and matches the largest
// frame the feasibility probe exercised).
const MaxPayload = 1 << 20

// headerLen is streamID(4) + type(1) + length(4).
const headerLen = 9

// Frame is a decoded multiplexed message.
type Frame struct {
	StreamID uint32
	Type     FrameType
	Payload  []byte
}

// valid reports whether t is a known frame type — a desynced/corrupt stream is
// unrecoverable, so ReadFrame rejects an unknown type rather than silently
// mis-routing bytes.
func (t FrameType) valid() bool {
	return t == FrameOpen || t == FrameData || t == FrameClose || t == FrameReady
}

// EncodeFrame serializes one framed message into a fresh, self-contained []byte
// (header+payload). Producers encode here and hand the bytes to a single
// dedicated writer goroutine (via a channel), so no producer ever holds a lock
// across the blocking pipe write — the relay and pump both funnel all frames
// through ONE writer goroutine rather than a shared write lock.
func EncodeFrame(streamID uint32, t FrameType, payload []byte) ([]byte, error) {
	if len(payload) > MaxPayload {
		return nil, fmt.Errorf("coop: frame payload %d exceeds max %d", len(payload), MaxPayload)
	}
	buf := make([]byte, headerLen+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], streamID)
	buf[4] = byte(t)
	binary.BigEndian.PutUint32(buf[5:9], uint32(len(payload)))
	copy(buf[headerLen:], payload)
	return buf, nil
}

// WriteFrame writes one framed message to w as a SINGLE Write (header+payload in
// one buffer) so a frame is never split at the codec layer. Concurrent WriteFrame
// calls on a shared writer MUST still be serialized by the caller.
func WriteFrame(w io.Writer, streamID uint32, t FrameType, payload []byte) error {
	buf, err := EncodeFrame(streamID, t, payload)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads one framed message from r (blocking). It enforces MaxPayload so
// a hostile length cannot exhaust memory, and rejects an unknown frame type
// (stream desync). io.EOF is returned verbatim when the stream ends cleanly at a
// frame boundary; a truncated frame yields io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [headerLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	streamID := binary.BigEndian.Uint32(hdr[0:4])
	t := FrameType(hdr[4])
	n := binary.BigEndian.Uint32(hdr[5:9])
	if !t.valid() {
		return Frame{}, fmt.Errorf("coop: unknown frame type %d (stream desync)", t)
	}
	if n > MaxPayload {
		return Frame{}, fmt.Errorf("coop: frame length %d exceeds max %d", n, MaxPayload)
	}
	var payload []byte
	if n > 0 {
		payload = make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, err
		}
	}
	return Frame{StreamID: streamID, Type: t, Payload: payload}, nil
}
