// Package watch implements project 9a (watch-view-surface): a host-agnostic,
// READ-ONLY "seventh terminal" TUI that mirrors the live governance loop. It
// owns invariant clauses 13-readonly (the ONLY new surface is read-only) and
// 12-readsurface (the combined trunk/integration state is observable in place).
//
// It is NOT safety-load-bearing (Inv: killing it or never starting it changes
// ZERO governed outcomes). The CARDINAL RULE is READ-ONLY BY CONSTRUCTION: this
// package reaches the daemon only through a client that can call read/query RPC
// methods, and a build-time grep test (readonly_test.go) fails if any MUTATING
// RPC method name appears in this package's source.
//
// Inv 10-decoupling: this package imports NEITHER the lease store /
// modernc.org/sqlite NOR any adapter/MCP package. It reads EVERYTHING via the
// daemon RPC over the socket; the decision-audit is read via the daemon's
// audit.tail method, never by touching the audit_log SQLite table.
package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/protocol"
)

// DefaultCallTimeout bounds EVERY read call (request+response) so a wedged or
// slow daemon can never freeze the TUI. A timed-out call yields a typed
// "unavailable" panel state, never a hang and never a crash.
const DefaultCallTimeout = 2 * time.Second

// readMethods enumerates the ONLY RPC methods this client may invoke. It is the
// runtime companion to the static grep test: a method not in this set cannot be
// dialed because the Client exposes no general-purpose Call. Every name here is
// a READ/QUERY method on the frozen daemon surface.
const (
	methodHealth          = "diag.health"
	methodWhoami          = "session.whoami"
	methodLeaseList       = "lease.list"
	methodIntegrateList   = "integrate.list"
	methodIntegrateTrunk  = "integrate.trunk"
	methodAuditTail       = "audit.tail"
	methodIntegrationList = "integration.list"
)

// Client is a READ-ONLY, deadline-bounded JSON-RPC client over the daemon's
// Unix socket. It exposes ONLY read methods: there is deliberately no exported
// generic Call, so no watch code path can reach a mutating method through it.
// The connection is serialized by mu (the TUI polls from one goroutine, but a
// refusing-then-recovering daemon must never interleave a half-written frame).
type Client struct {
	socket  string
	timeout time.Duration

	mu     sync.Mutex
	conn   net.Conn
	dec    *json.Decoder
	enc    *json.Encoder
	id     int
	closed bool // set by Close; a closed client never re-dials
}

// Dial connects to the daemon at socket. A failure to connect is returned to
// the caller (the model surfaces a friendly "cannot reach daemon" screen); it
// is never fatal. timeout<=0 uses DefaultCallTimeout.
func Dial(socket string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	conn, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return nil, err
	}
	return newClient(socket, timeout, conn), nil
}

func newClient(socket string, timeout time.Duration, conn net.Conn) *Client {
	return &Client{
		socket:  socket,
		timeout: timeout,
		conn:    conn,
		dec:     json.NewDecoder(conn),
		enc:     json.NewEncoder(conn),
	}
}

// Socket returns the socket path this client targets (for the friendly
// unreachable message).
func (c *Client) Socket() string { return c.socket }

// Close closes the underlying connection. The watch client holds NO leases and
// makes NO mutations, so there is nothing to release.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.dec = nil
	c.enc = nil
	return err
}

// call invokes a read method with a per-call read+write deadline. It is
// UNEXPORTED and the method name is supplied only by this package's typed
// wrappers, so no external caller can route a mutating method through it.
//
// SELF-HEALING: on ANY transport error (dial / deadline / encode / decode) the
// connection is DROPPED and the next call lazily re-dials. This is load-bearing:
// a json.Decoder that hits a read deadline caches that error permanently
// (encoding/json stores it in dec.err), so a single slow tick would otherwise
// poison the stream forever and strand the TUI on "unavailable" until restart.
// Dropping + re-dialing makes a transient daemon slowness/outage recover on the
// next poll. The lazy dial is bounded by the same per-call timeout, so it can
// never hang. Tolerant of shape mismatch: out is unmarshaled leniently and
// unknown extra fields are ignored.
func (c *Client) call(method string, params any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("watch: client closed")
	}

	// Lazily (re)establish the connection if a prior call dropped it.
	if c.conn == nil {
		conn, err := net.DialTimeout("unix", c.socket, c.timeout)
		if err != nil {
			return fmt.Errorf("watch: dial %s: %w", c.socket, err)
		}
		c.conn = conn
		c.dec = json.NewDecoder(conn)
		c.enc = json.NewEncoder(conn)
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
	// the read decode below.
	deadline := time.Now().Add(c.timeout)
	if err := c.conn.SetDeadline(deadline); err != nil {
		c.dropConn()
		return err
	}
	if err := c.enc.Encode(&req); err != nil {
		c.dropConn()
		return fmt.Errorf("watch: %s send: %w", method, err)
	}

	// Read until OUR response id returns, skipping any stale frame a prior call
	// may have left on a reused connection (defence-in-depth against stream
	// desync: with drop-on-error the connection is normally clean, but a late
	// reply must NEVER be mis-attributed to a later method). The deadline bounds
	// the whole loop; any decode error drops the connection so the next call
	// re-dials a clean stream.
	for {
		var resp protocol.Response
		if err := c.dec.Decode(&resp); err != nil {
			c.dropConn()
			return fmt.Errorf("watch: %s recv: %w", method, err)
		}
		if !responseIDIs(resp.ID, reqID) {
			continue // stale / unmatched frame — skip it
		}
		if resp.Error != nil {
			return fmt.Errorf("watch: %s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
		}
		if out != nil && len(resp.Result) > 0 {
			// Lenient: a garbled or shape-mismatched result degrades to zero values
			// per field; unknown extra fields are ignored by encoding/json.
			_ = json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}

// dropConn closes and clears the connection so the next call lazily re-dials a
// clean stream. Called with mu held, on any transport error.
func (c *Client) dropConn() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
	c.dec = nil
	c.enc = nil
}

// responseIDIs reports whether a JSON-RPC response id equals the integer request
// id we sent. A nil/garbled id never matches (the frame is skipped).
func responseIDIs(id *json.RawMessage, want int) bool {
	if id == nil {
		return false
	}
	return strings.TrimSpace(string(*id)) == strconv.Itoa(want)
}

// --- typed read wrappers — the ONLY surface watch code may call --------------

// HealthInfo mirrors diag.health (read-only).
type HealthInfo struct {
	PID             int    `json:"pid"`
	UptimeSeconds   int64  `json:"uptime_seconds"`
	SocketPath      string `json:"socket_path"`
	ActiveConns     int64  `json:"active_connections"`
	ContractVersion int    `json:"contract_version"`
}

// Health calls diag.health (process/socket health only).
func (c *Client) Health() (HealthInfo, error) {
	var out HealthInfo
	err := c.call(methodHealth, nil, &out)
	return out, err
}

// Whoami calls session.whoami (this connection's daemon-minted identity).
func (c *Client) Whoami() (string, error) {
	var out struct {
		Session string `json:"session"`
	}
	err := c.call(methodWhoami, nil, &out)
	return out.Session, err
}

// LeaseHolder mirrors one lease.list entry (read-only).
type LeaseHolder struct {
	Key         string `json:"key"`
	Holder      string `json:"holder"`
	ExpiresAtMs int64  `json:"expires_at_ms"`
	Fence       int64  `json:"fence"`
}

// LeaseList calls lease.list (currently-held leases).
func (c *Client) LeaseList() ([]LeaseHolder, error) {
	var out struct {
		Holders []LeaseHolder `json:"holders"`
	}
	err := c.call(methodLeaseList, nil, &out)
	return out.Holders, err
}

// Integration mirrors one integrate.list entry (read-only).
type Integration struct {
	ID          string `json:"id"`
	Branch      string `json:"branch"`
	Holder      string `json:"holder"`
	State       string `json:"state"`
	Base        string `json:"base"`
	MergeCommit string `json:"merge_commit"`
}

// IntegrateList calls integrate.list (all integrations).
func (c *Client) IntegrateList() ([]Integration, error) {
	var out struct {
		Integrations []Integration `json:"integrations"`
	}
	err := c.call(methodIntegrateList, nil, &out)
	return out.Integrations, err
}

// TrunkRef mirrors integrate.trunk (read-only): the AUTHORITATIVE trunk tip (the
// commit the integrator's CAS last advanced), mirrored directly rather than
// derived from the integrate.list ordering.
type TrunkRef struct {
	Tip    string `json:"tip"`
	Exists bool   `json:"exists"`
	Branch string `json:"branch"`
}

// TrunkRef calls integrate.trunk.
func (c *Client) TrunkRef() (TrunkRef, error) {
	var out TrunkRef
	err := c.call(methodIntegrateTrunk, nil, &out)
	return out, err
}

// IntegrationRecord mirrors one integration.list entry (read-only): a Wing-3
// REVIEW-queue record. State is one of requested/claimed/changes_requested/
// approved/withdrawn. Feedback carries reviewer notes on a changes_requested
// verdict; Merge carries the merge OID on an approved verdict.
type IntegrationRecord struct {
	ID          string `json:"id"`
	Branch      string `json:"branch"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Claimer     string `json:"claimer"`
	Feedback    string `json:"feedback"`
	Merge       string `json:"merge"`
	CreatedAtMs int64  `json:"created_at_ms"`
	UpdatedAtMs int64  `json:"updated_at_ms"`
}

// IntegrationList calls integration.list (all REVIEW-queue records, any state).
// It is READ-ONLY; like its siblings it is fail-soft — a transport error drops
// the connection (the next call re-dials) and the panel degrades to unavailable.
func (c *Client) IntegrationList() ([]IntegrationRecord, error) {
	var out struct {
		Records []IntegrationRecord `json:"records"`
	}
	err := c.call(methodIntegrationList, nil, &out)
	return out.Records, err
}

// AuditEntry mirrors one audit.tail record (read-only), newest-first.
type AuditEntry struct {
	TimestampMs     int64           `json:"timestamp_ms"`
	Session         string          `json:"session"`
	DecisionProject string          `json:"decision_project"`
	DecisionKind    string          `json:"decision_kind"`
	Payload         json.RawMessage `json:"payload"`
}

// AuditTail calls audit.tail (newest-first decision-audit records).
func (c *Client) AuditTail(limit int) ([]AuditEntry, error) {
	var out struct {
		Records []AuditEntry `json:"records"`
	}
	err := c.call(methodAuditTail, map[string]int{"limit": limit}, &out)
	return out.Records, err
}
