// Package protocol defines the FROZEN mad-substrate daemon wire contract: JSON-RPC
// 2.0 envelopes over a Unix socket, a stable error taxonomy, and an explicit
// contract version on every envelope. This is the load-bearing interface every
// other project codes against (docs/0003 project 1 "daemon-arbiter-protocol";
// docs/0004 card 1).
//
// Inv 10-decoupling: the agent-facing MCP dialect is deliberately NOT defined
// here — it lives only in the host adapter (project 9b). This package couples
// to no agent/host/orchestrator.
package protocol

import (
	"encoding/json"
	"fmt"
)

// ContractVersion is the mad-substrate daemon contract version, carried on every
// envelope as the "v" field (distinct from the JSON-RPC "jsonrpc" field).
// Bumping it is a breaking change to the frozen contract and requires re-review.
const ContractVersion = 1

// JSONRPCVersion is the JSON-RPC protocol version string.
const JSONRPCVersion = "2.0"

// Request is an incoming JSON-RPC 2.0 request envelope.
type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	V       int              `json:"v"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// Response is an outgoing JSON-RPC 2.0 response envelope. Exactly one of Result
// or Error is set.
type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	V       int              `json:"v"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
}

// Error is a JSON-RPC error object drawn from the stable taxonomy below.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// The stable error taxonomy (docs/0004 card 1: client-fault, authz-fault,
// not-found, conflict/CAS-fail, internal, not-implemented). These codes are
// part of the frozen contract; changing one requires re-review.
const (
	// Protocol-level (JSON-RPC reserved range).
	CodeParse          = -32700 // malformed JSON
	CodeInvalidRequest = -32600 // envelope failed validation (bad version, etc.)
	CodeMethodNotFound = -32601 // no such registered method
	CodeInvalidParams  = -32602 // client-fault: params failed validation
	CodeInternal       = -32603 // internal-fault

	// Application taxonomy.
	CodeAuthz          = -32001 // authz-fault: caller not permitted
	CodeNotFound       = -32002 // not-found: addressed entity absent
	CodeConflict       = -32003 // conflict / CAS-fail (e.g. lease held)
	CodeNotImplemented = -32004 // canonical not-implemented stub
)

// NewError builds an Error from the taxonomy.
func NewError(code int, msg string) *Error { return &Error{Code: code, Message: msg} }

// ErrNotImplemented is the canonical not-implemented stub error returned by a
// registered-but-unimplemented method (used to publish a frozen signature
// before its body exists).
var ErrNotImplemented = NewError(CodeNotImplemented, "not implemented")

// Validate checks the frozen envelope invariants: jsonrpc=="2.0", a compatible
// contract version, and a non-empty method. Returns nil when acceptable.
func (r *Request) Validate() *Error {
	if r.JSONRPC != JSONRPCVersion {
		return NewError(CodeInvalidRequest, `jsonrpc must be "2.0"`)
	}
	if r.V != ContractVersion {
		return NewError(CodeInvalidRequest, fmt.Sprintf("unsupported contract version %d (want %d)", r.V, ContractVersion))
	}
	if r.Method == "" {
		return NewError(CodeInvalidRequest, "method required")
	}
	return nil
}
