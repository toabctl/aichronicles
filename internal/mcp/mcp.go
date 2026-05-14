// Package mcp is a minimal implementation of the Model Context
// Protocol server side, scoped to what aichronicles needs today:
// JSON-RPC 2.0 framing over stdio, the MCP handshake, and a small set
// of read-only tools (commit 2).
//
// We deliberately do not depend on an external MCP SDK:
//   - the protocol surface we use is small and stable
//   - keeping the dependency set tight (only stdlib + a few leaf
//     packages already present) matches the rest of the tree
//
// Spec reference: https://modelcontextprotocol.io/specification —
// we target protocol version "2024-11-05".
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// ProtocolVersion is the MCP revision this server negotiates with.
// Clients ask for a specific version during `initialize`; we echo this
// one back. Bumping requires a compatibility audit.
const ProtocolVersion = "2024-11-05"

// ServerInfo is the identity broadcast during `initialize`. Kept
// small and honest — clients log it verbatim.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Capabilities is the subset of MCP server capabilities we advertise.
// Only `tools` for now; resources + prompts + sampling are future
// work. Each capability block carries feature flags (listChanged,
// etc.) — we ship a conservative minimum.
type Capabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

// ToolsCapability declares server-side tool support. listChanged is
// false because our tool set is static for the lifetime of the
// process; a future dynamic registry would flip this to true.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// Server is the MCP endpoint. One instance per process. Safe to
// share across goroutines for the duration of Run (the handler
// dispatch is serial, but method handlers may be re-entrant).
type Server struct {
	Info ServerInfo
	Log  *slog.Logger

	// handlers map JSON-RPC method names to their dispatch functions.
	// Populated by registerBuiltins() in New; downstream packages add
	// tool methods via RegisterTool.
	handlers map[string]methodHandler

	// tools is the registered tool set. Populated via RegisterTool;
	// handlers for tools/list and tools/call are lazily wired the
	// first time a tool is registered.
	tools map[string]Tool

	mu          sync.RWMutex
	initialized bool
}

// methodHandler is the internal dispatcher signature. Handlers return
// the raw result (marshaled by the caller) or an *Error for JSON-RPC
// error objects.
type methodHandler func(ctx context.Context, params json.RawMessage) (any, *Error)

// New builds a Server with the MCP built-in methods installed but no
// tools registered yet. log must be non-nil — pass
// slog.New(slog.DiscardHandler) from a test that doesn't care about
// log output. Explicit logger injection avoids the silent-default
// fallback that masked a logger-not-wired bug in commit 89a3deb.
func New(info ServerInfo, log *slog.Logger) *Server {
	if log == nil {
		panic("mcp.New: log must be non-nil")
	}
	s := &Server{
		Info:     info,
		Log:      log,
		handlers: map[string]methodHandler{},
	}
	s.registerBuiltins()
	return s
}

// registerBuiltins wires the handshake + lifecycle methods every MCP
// server MUST implement. Tool-specific dispatch (`tools/list`,
// `tools/call`) is commit 2's job; leaving the names undefined here
// keeps commit 1 to the bare transport.
func (s *Server) registerBuiltins() {
	s.handlers["initialize"] = s.handleInitialize
	s.handlers["initialized"] = s.handleInitialized // notification, no response
	s.handlers["ping"] = s.handlePing
	s.handlers["shutdown"] = s.handleShutdown
}

// Run reads requests from r, dispatches them, and writes responses to
// w until r closes or ctx is cancelled. Errors from individual
// requests are surfaced as JSON-RPC error objects — not as Run
// failures — so one bad request never kills the process.
//
// The wire framing is line-delimited JSON (one request per line),
// which matches how Claude Desktop and every other MCP stdio client
// speak today. The spec also allows the Content-Length header style;
// we can add that later if needed.
func (s *Server) Run(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	// MCP requests can be large (tool call args with prompts). Bump
	// the default 64K scanner buffer to 4MB — well above anything
	// realistic, below "runaway input".
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)

	enc := json.NewEncoder(w)
	var encMu sync.Mutex // serializes writes; Encode is not goroutine-safe

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			s.Log.Warn("mcp: malformed request", "err", err)
			encMu.Lock()
			_ = enc.Encode(errorResponse(nil, ParseError, "parse error: "+err.Error(), nil))
			encMu.Unlock()
			continue
		}
		if req.JSONRPC != "2.0" {
			encMu.Lock()
			_ = enc.Encode(errorResponse(req.ID, InvalidRequest, "jsonrpc must be \"2.0\"", nil))
			encMu.Unlock()
			continue
		}

		resp := s.dispatch(ctx, &req)
		// Notifications (no id) get no response by JSON-RPC rules.
		if req.ID == nil {
			continue
		}
		encMu.Lock()
		err := enc.Encode(resp)
		encMu.Unlock()
		if err != nil {
			return fmt.Errorf("mcp: encode response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("mcp: read input: %w", err)
	}
	return nil
}

// dispatch looks up the method and builds the JSON-RPC response. A
// missing method yields a -32601 Method Not Found error.
func (s *Server) dispatch(ctx context.Context, req *Request) *Response {
	handler, ok := s.handlers[req.Method]
	if !ok {
		return errorResponse(req.ID, MethodNotFound, "method not found: "+req.Method, nil)
	}
	result, mcpErr := handler(ctx, req.Params)
	if mcpErr != nil {
		return errorResponse(req.ID, mcpErr.Code, mcpErr.Message, mcpErr.Data)
	}
	return successResponse(req.ID, result)
}

// --- built-in method handlers ---

// handleInitialize negotiates protocol version + capabilities with
// the client. MCP requires we also record that the client completed
// the handshake; that happens in handleInitialized.
func (s *Server) handleInitialize(_ context.Context, params json.RawMessage) (any, *Error) {
	var req InitializeRequest
	if len(params) > 0 {
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &Error{Code: InvalidParams, Message: "bad initialize params: " + err.Error()}
		}
	}
	// Per spec we echo our own protocol version, not the client's —
	// it lets a newer client downshift cleanly to our older support.
	return InitializeResult{
		ProtocolVersion: ProtocolVersion,
		ServerInfo:      s.Info,
		Capabilities: Capabilities{
			Tools: &ToolsCapability{ListChanged: false},
		},
	}, nil
}

// handleInitialized is the client's "ack". It's a notification, so
// we return nil/nil — Run() won't emit a response for it.
func (s *Server) handleInitialized(_ context.Context, _ json.RawMessage) (any, *Error) {
	s.mu.Lock()
	s.initialized = true
	s.mu.Unlock()
	return nil, nil
}

// handlePing is the canonical health check. Empty object reply.
func (s *Server) handlePing(_ context.Context, _ json.RawMessage) (any, *Error) {
	return struct{}{}, nil
}

// handleShutdown acknowledges the client's intent to close. We don't
// exit the process — the caller is expected to close stdin, which
// ends Run() naturally.
func (s *Server) handleShutdown(_ context.Context, _ json.RawMessage) (any, *Error) {
	return struct{}{}, nil
}

// Initialized reports whether a client has completed the handshake.
// Exported for tests and future tool handlers that might want to
// refuse calls pre-handshake.
func (s *Server) Initialized() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.initialized
}
