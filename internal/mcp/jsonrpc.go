package mcp

import "encoding/json"

// JSON-RPC 2.0 standard error codes. MCP reserves a few more in the
// -32000..-32099 range but we don't emit those yet.
const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
)

// Request is a single JSON-RPC 2.0 request. ID is left as RawMessage
// so we preserve the exact shape the client used — the spec allows
// strings, numbers, or null, and echoing the exact bytes avoids
// any subtle type drift.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is the success-or-error envelope. Exactly one of Result
// and Error should be set; the JSON encoding uses omitempty to keep
// the on-the-wire shape clean.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is the JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// InitializeRequest mirrors the MCP initialize-params shape. We only
// decode the bits we act on; extra fields are ignored. Clients will
// send protocolVersion + clientInfo + capabilities; we echo our own
// values back in InitializeResult.
type InitializeRequest struct {
	ProtocolVersion string `json:"protocolVersion"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

// InitializeResult is the server's side of the handshake.
type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
	Capabilities    Capabilities `json:"capabilities"`
}

// successResponse builds a 2.0 response carrying a result payload.
func successResponse(id json.RawMessage, result any) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Result: result}
}

// errorResponse builds a 2.0 response carrying an error object.
// id may be nil (e.g. parse error before the id was recovered).
func errorResponse(id json.RawMessage, code int, message string, data any) *Response {
	if id == nil {
		// MUST still emit `"id": null` on the wire.
		id = json.RawMessage("null")
	}
	return &Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &Error{Code: code, Message: message, Data: data},
	}
}
