// Package rpcclient is a thin JSON-RPC 2.0 client over the daemon's Unix socket
// — used by out-of-process CLI commands (integrate, trunk, gate, recover,
// daemon_ctl, watch) to reach the frozen daemon surface.
//
// HARDENING (chafe C9 + C22): a wedged daemon must never freeze a Go client.
// Every Call is bounded by a per-call read+write deadline, and on ANY transport
// error (write error, read error, OR a deadline breach) the connection is
// DROPPED so the NEXT Call lazily re-dials a fresh connection — a json.Decoder
// that hits a read deadline caches that error for the life of the connection
// (encoding/json stores it in dec.err), so a single slow tick would otherwise
// POISON the stream permanently. Dropping + re-dialing makes a transient daemon
// slowness/outage self-heal on the next call. Responses are matched by request
// id, so a late reply on a reused connection can never be mis-attributed to a
// later Call (C22). This mirrors internal/watch/client.go exactly; every Go CLI
// gets the hardening for free through the preserved Dial/Call/Close API.
package rpcclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/protocol"
)

// DefaultDialTimeout bounds the initial connect and every lazy re-dial.
const DefaultDialTimeout = 3 * time.Second

// DefaultReadTimeout bounds EACH Call (write encode + read decode): a wedged
// daemon can never freeze a client past this — the Call errors and the conn is
// dropped so the next Call re-dials a clean stream (bounding C9 at a finite wait
// instead of forever). It is deliberately GENEROUS: it matches the integrator's
// 120s promote-lease TTL, the system's max budget for a single operation. A
// legitimately slow RPC must not be false-timed-out — integrate.promote runs
// `git merge-tree`/`commit-tree`/`update-ref` SYNCHRONOUSLY inside the round-trip
// and is budgeted to that TTL. Interactive/polling surfaces that want snappier
// wedged-daemon detection (e.g. the watch TUI) use their own shorter deadline;
// any caller may pass WithReadTimeout for a tighter bound.
const DefaultReadTimeout = 120 * time.Second

// Client is a JSON-RPC client over the daemon's Unix socket. The socket path is
// REMEMBERED so the connection can be lazily (re)established: after any
// transport error the conn is dropped and the next Call re-dials.
//
// NOTE on reuse: a reconnect only happens after a transport ERROR (which already
// tears down any connection-scoped daemon state, e.g. a held lease). On the
// happy path the SAME persistent connection is reused across Calls, so leases
// and the daemon-minted session identity are preserved exactly as before.
type Client struct {
	socket      string
	dialTimeout time.Duration
	readTimeout time.Duration

	conn   net.Conn
	dec    *json.Decoder
	enc    *json.Encoder
	id     int
	closed bool // set by Close; a closed client never re-dials
}

// Option configures a Client at Dial time. Options are additive so existing
// call sites — rpcclient.Dial(socket) — compile and behave unchanged.
type Option func(*Client)

// WithReadTimeout overrides the per-call read+write deadline (<=0 keeps the
// default).
func WithReadTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.readTimeout = d
		}
	}
}

// WithDialTimeout overrides the connect / re-dial timeout (<=0 keeps the
// default).
func WithDialTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.dialTimeout = d
		}
	}
}

// Dial connects to the daemon at the given Unix socket. The returned Client
// remembers the socket so it can transparently re-dial after a transport error.
func Dial(socket string, opts ...Option) (*Client, error) {
	c := &Client{
		socket:      socket,
		dialTimeout: DefaultDialTimeout,
		readTimeout: DefaultReadTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	conn, err := net.DialTimeout("unix", socket, c.dialTimeout)
	if err != nil {
		return nil, err
	}
	c.setConn(conn)
	return c, nil
}

func (c *Client) setConn(conn net.Conn) {
	c.conn = conn
	c.dec = json.NewDecoder(conn)
	c.enc = json.NewEncoder(conn)
}

// dropConn closes and clears the connection so the next Call lazily re-dials a
// clean stream. Called on any transport error: a poisoned decoder must never be
// reused.
func (c *Client) dropConn() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
	c.dec = nil
	c.enc = nil
}

// Call invokes method with params and unmarshals the result into out (nil to
// ignore). A protocol error is returned as a Go error.
//
// On ANY transport error (lazy dial, deadline breach on the write or read,
// encode, or decode) the connection is dropped before returning, so the next
// Call re-dials a fresh connection. The response is matched by request id; a
// stale frame left by a prior timed-out Call on a reused connection is skipped
// rather than mis-attributed (C22).
func (c *Client) Call(method string, params any, out any) error {
	return c.call(method, params, out, c.readTimeout)
}

// CallWithReadTimeout is Call with a one-shot read+write deadline override (<=0 uses
// the client default). The launcher keepalive uses it to bound a renew/reattach FAR
// below the default 120s so a WEDGED daemon (one that accepts but never replies) can
// neither defeat re-attach recovery (which must land within the session-liveness
// lease TTL) nor stall clean-exit teardown past its budget. Like Call, it is NOT safe
// for concurrent use with another Call on the same client (callers serialize).
func (c *Client) CallWithReadTimeout(method string, params, out any, d time.Duration) error {
	if d <= 0 {
		d = c.readTimeout
	}
	return c.call(method, params, out, d)
}

func (c *Client) call(method string, params any, out any, timeout time.Duration) error {
	if c.closed {
		return ErrClosed
	}
	// Lazily (re)establish the connection if a prior Call dropped it.
	if c.conn == nil {
		conn, err := net.DialTimeout("unix", c.socket, c.dialTimeout)
		if err != nil {
			return fmt.Errorf("rpcclient: dial %s: %w", c.socket, err)
		}
		c.setConn(conn)
	}

	c.id++
	reqID := c.id
	idRaw := json.RawMessage(strconv.Itoa(reqID))
	req := protocol.Request{JSONRPC: protocol.JSONRPCVersion, V: protocol.ContractVersion, ID: &idRaw, Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = b
	}

	// Bound BOTH directions: a daemon that accepts but never reads, or reads but
	// never replies, must not wedge us. SetDeadline covers the write encode and
	// the read decode loop below.
	deadline := time.Now().Add(timeout)
	if err := c.conn.SetDeadline(deadline); err != nil {
		c.dropConn()
		return err
	}
	if err := c.enc.Encode(&req); err != nil {
		c.dropConn()
		return fmt.Errorf("rpc %s: send: %w", method, err)
	}

	// Read until OUR response id returns, skipping any stale frame a prior Call
	// may have left on a reused connection. The deadline bounds the whole loop;
	// any decode error drops the connection so the next Call re-dials.
	for {
		var resp protocol.Response
		if err := c.dec.Decode(&resp); err != nil {
			c.dropConn()
			return fmt.Errorf("rpc %s: recv: %w", method, err)
		}
		if !responseIDIs(resp.ID, reqID) {
			continue // stale / unmatched frame — skip it
		}
		if resp.Error != nil {
			return fmt.Errorf("rpc %s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
		}
		if out != nil {
			if len(resp.Result) == 0 {
				return nil
			}
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}

// Close closes the connection (releasing any leases held by this session is the
// caller's responsibility before Close).
func (c *Client) Close() error {
	c.closed = true
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.dropConn()
	return err
}

// responseIDIs reports whether a JSON-RPC response id equals the integer request
// id we sent. A nil/garbled id never matches (the frame is skipped).
func responseIDIs(id *json.RawMessage, want int) bool {
	if id == nil {
		return false
	}
	return strings.TrimSpace(string(*id)) == strconv.Itoa(want)
}

// ErrClosed is returned by Call after Close: a deliberately torn-down client
// never re-dials (mirrors the watch client's closed-guard).
var ErrClosed = errors.New("rpcclient: client closed")
