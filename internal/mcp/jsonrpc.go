package mcp

import "encoding/json"

// JSON-RPC 2.0 error codes used by this server. -32700/-32601 are standard;
// tool FAILURES never use these (they return an MCP isError result, not a
// JSON-RPC error) — only malformed input and unknown methods do.
const (
	codeParseError     = -32700
	codeMethodNotFound = -32601
)

// rpcRequest is an incoming MCP JSON-RPC 2.0 message. ID is a raw message so we
// can echo it back byte-for-byte (numbers and strings are both valid ids) and
// so we can distinguish a present id (request) from an absent one
// (notification): a notification omits the field entirely, leaving ID nil.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is an outgoing JSON-RPC 2.0 response. Exactly one of Result /
// Error is set. ID is echoed from the request (raw, so it round-trips exactly).
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// resultResponse builds a success response carrying result for the given id.
func resultResponse(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: idOrNull(id), Result: result}
}

// errorResponse builds an error response for the given id.
func errorResponse(id json.RawMessage, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: idOrNull(id), Error: &rpcError{Code: code, Message: msg}}
}

// idOrNull normalizes a missing id to JSON null so a response always carries an
// "id" field (required by JSON-RPC 2.0 even on errors with no recoverable id).
func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// salvageID attempts to pull an "id" out of an otherwise-unparseable line so a
// parse-error response can be correlated by the client. It returns ok=false when
// no usable id can be recovered (then the line is dropped, not answered).
func salvageID(line []byte) (json.RawMessage, bool) {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, false
	}
	if len(probe.ID) == 0 {
		return nil, false
	}
	return probe.ID, true
}
