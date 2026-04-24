package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/toabctl/aichronicles/internal/redact"
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
// iteration order today — clients should not depend on it; a future
// enhancement could sort by name for determinism.
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
	return map[string]any{"tools": list}, nil
}

// handleToolsCall dispatches to a specific tool. All text emitted to
// the client passes through redact.Outbound — a defense-in-depth
// copy of the egress scrub Block B applied for LLM calls. Even for
// read-only tools hitting already-scrubbed data, re-scanning at the
// egress boundary means a detector added AFTER the data landed still
// works.
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
	// Scrub every text block before handing it back to the client.
	// This is cheap (regex scan) relative to the DB query that
	// produced the content.
	for i, c := range result.Content {
		if c.Type != "text" || c.Text == "" {
			continue
		}
		scrubbed, _ := redact.Outbound(c.Text)
		result.Content[i].Text = scrubbed
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
