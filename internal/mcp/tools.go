package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Tool is one registered tool. Handler is invoked with the raw
// arguments blob the client sent; return either a ToolResult (content
// blocks + optional isError) or an *Error for protocol-level
// failures (malformed args, missing store, etc.).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`

	Handler ToolHandler `json:"-"`
}

// ToolHandler is the user-supplied dispatcher for a tool call. args
// is whatever the client passed under "arguments"; it's callers'
// responsibility to Unmarshal into their specific shape.
type ToolHandler func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error)

// ToolResult matches MCP's `tools/call` response shape. Content is a
// list of blocks; today we only emit text blocks. IsError=true tells
// the client the call ran but produced a user-facing error (not a
// protocol error).
type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ToolContent is one piece of a tool's reply. MCP supports text,
// image, and resource blocks; we emit text-only.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// RegisterTool wires name→handler and schema into the server. Safe
// to call before Run starts; calling concurrently with Run is not
// supported.
func (s *Server) RegisterTool(t Tool) {
	if s.tools == nil {
		s.tools = map[string]Tool{}
	}
	if _, dup := s.tools[t.Name]; dup {
		panic("mcp: duplicate tool registration: " + t.Name)
	}
	s.tools[t.Name] = t

	// Wire the JSON-RPC method handlers the first time a tool is
	// registered. Idempotent by map identity.
	if _, ok := s.handlers["tools/list"]; !ok {
		s.handlers["tools/list"] = s.handleToolsList
		s.handlers["tools/call"] = s.handleToolsCall
	}
}

// handleToolsList returns the registered tools. Ordering is map
// Sorted by name. Map iteration order is randomised in Go, so an
// unsorted list made client-side logs and any snapshot test unstable
// for no benefit.
func (s *Server) handleToolsList(_ context.Context, _ json.RawMessage) (any, *Error) {
	list := make([]Tool, 0, len(s.tools))
	for _, t := range s.tools {
		// Strip the handler field before serializing (json:"-"
		// already handles that, but being explicit future-proofs
		// against a sibling-field addition).
		list = append(list, Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return map[string]any{"tools": list}, nil
}

// handleToolsCall dispatches to a specific tool and returns its
// ToolResult verbatim. Content text is NOT scrubbed here — every
// envelope that reaches the store has already passed through the
// edge redactor (rejected by the daemon otherwise), and re-scanning
// on every read just doubled the per-byte cost without catching
// anything ingest hadn't seen. Detector-set changes are handled
// operationally by `aichronicles scrub`, which rewrites stored
// rows in place; see docs/explanation/threat-model.md.
func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *Error) {
	var req struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &Error{Code: InvalidParams, Message: "bad tools/call params: " + err.Error()}
	}
	tool, ok := s.tools[req.Name]
	if !ok {
		return nil, &Error{Code: MethodNotFound, Message: "unknown tool: " + req.Name}
	}
	result, mcpErr := tool.Handler(ctx, req.Arguments)
	if mcpErr != nil {
		return nil, mcpErr
	}
	if result == nil {
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: ""}}}, nil
	}
	return result, nil
}

// TextResult is a convenience for tools whose reply is a single
// text block. Avoids boilerplate at every handler site.
func TextResult(text string) *ToolResult {
	return &ToolResult{Content: []ToolContent{{Type: "text", Text: text}}}
}

// TextError wraps an error message as a user-facing tool result (not
// a protocol error). Use this when the tool ran but the user's input
// was unusable — e.g. an unknown session_id.
func TextError(format string, args ...any) *ToolResult {
	return &ToolResult{
		Content: []ToolContent{{Type: "text", Text: fmt.Sprintf(format, args...)}},
		IsError: true,
	}
}
