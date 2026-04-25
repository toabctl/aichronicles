package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
)

// openSeededStore writes a handful of events across two sessions so
// search_events, list_sessions, and get_summary each have real data
// to return.
func openSeededStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	now := time.Now().UTC()
	fixtures := []struct {
		sess, kind, content string
		off                 int
	}{
		{"sess-foo", "user_prompt", "how do I parse jsonl in Go", 0},
		{"sess-foo", "assistant_message", "bufio.Scanner works for jsonl", 1},
		{"sess-bar", "user_prompt", "explain systemd socket activation", 0},
		{"sess-bar", "assistant_message", "LISTEN_FDS + fd 3", 1},
	}
	for _, fx := range fixtures {
		env := ingest.Envelope{
			V:               1,
			EventID:         uuid.Must(uuid.NewV7()).String(),
			SourceAgent:     "claude-code",
			SourceSessionID: fx.sess,
			Kind:            fx.kind,
			Role:            "user",
			TsSource:        now.Add(time.Duration(fx.off) * time.Second),
			Cwd:             "/work/" + fx.sess,
			ContentText:     fx.content,
			Payload:         map[string]any{"k": fx.kind},
			Redaction:       &ingest.Redaction{Applied: true},
		}
		raw, _ := json.Marshal(env)
		tx, _ := s.DB().Begin()
		if _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			t.Fatalf("seed: %v", err)
		}
		_ = tx.Commit()
	}
	return s
}

// callTool invokes tool.Handler directly without going through the
// JSON-RPC layer. Keeps these tests tight and deterministic — the
// wire path is exercised by mcp_test.go.
func callTool(t *testing.T, s *Server, name string, args string) *ToolResult {
	t.Helper()
	tool, ok := s.tools[name]
	if !ok {
		t.Fatalf("tool %q not registered", name)
	}
	res, mcpErr := tool.Handler(context.Background(), json.RawMessage(args))
	if mcpErr != nil {
		t.Fatalf("tool %s: protocol error: %+v", name, mcpErr)
	}
	return res
}

func TestRegisterAichroniclesTools_InstallsAllThree(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	for _, want := range []string{"search_events", "list_sessions", "get_summary"} {
		if _, ok := s.tools[want]; !ok {
			t.Errorf("tool %q not registered", want)
		}
	}
}

func TestSearchEvents_FindsMatchingRow(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "search_events", `{"query":"jsonl"}`)
	if res.IsError {
		t.Fatalf("expected success, got %+v", res)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "jsonl") {
		t.Errorf("result missing match:\n%s", text)
	}
}

func TestSearchEvents_MissingQueryReturnsUserFacingError(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "search_events", `{}`)
	if !res.IsError {
		t.Errorf("expected IsError=true for missing query")
	}
}

// TestSearchEvents_PrefixMatchFromBareToken proves the agent no
// longer needs to know FTS5 syntax: a bare token like "json"
// matches "jsonl" in the seeded data because the parser appends *.
func TestSearchEvents_PrefixMatchFromBareToken(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "search_events", `{"query":"json"}`)
	if res.IsError {
		t.Fatalf("expected success, got %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "jsonl") {
		t.Errorf("prefix match failed: %s", res.Content[0].Text)
	}
}

// TestSearchEvents_PunctuationDoesNotError confirms a query that
// would have been an FTS5 syntax error before — bare punctuation,
// embedded specials — now returns either a clean user error or no
// hits, never a 500.
func TestSearchEvents_PunctuationDoesNotError(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	for _, q := range []string{
		`{"query":"foo*bar"}`,
		`{"query":"foo(bar"}`,
		`{"query":"-leading-dash"}`,
	} {
		res := callTool(t, s, "search_events", q)
		// Either no hits or a clean user error is fine — what we're
		// guarding against is an opaque SQLite parse error bubbling
		// out as a JSON-RPC InternalError.
		if !res.IsError && res.Content[0].Text == "" {
			t.Errorf("query %s: empty success response", q)
		}
	}
}

// TestSearchEvents_UnclosedQuoteIsUserError verifies the parser's
// ErrSyntax surfaces as a user-facing tool error, not a protocol
// error.
func TestSearchEvents_UnclosedQuoteIsUserError(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "search_events", `{"query":"find \"this without close"}`)
	if !res.IsError {
		t.Fatalf("expected IsError=true for unclosed quote")
	}
	if !strings.Contains(res.Content[0].Text, "unclosed quote") {
		t.Errorf("expected diagnostic to mention unclosed quote: %s", res.Content[0].Text)
	}
}

func TestListSessions_ReturnsSessionRows(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "list_sessions", `{}`)
	text := res.Content[0].Text
	// Both sessions should appear.
	for _, want := range []string{"/work/sess-foo", "/work/sess-bar"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected %q in output:\n%s", want, text)
		}
	}
}

func TestListSessions_CwdFilterNarrows(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "list_sessions", `{"cwd":"/work/sess-foo"}`)
	text := res.Content[0].Text
	if strings.Contains(text, "/work/sess-bar") {
		t.Errorf("cwd filter leaked: %s", text)
	}
	if !strings.Contains(text, "/work/sess-foo") {
		t.Errorf("expected /work/sess-foo in output:\n%s", text)
	}
}

func TestGetSummary_ReturnsStoredBody(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	sessID := ingest.DeriveSessionID("claude-code", "sess-foo")
	tx, _ := st.DB().Begin()
	_, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   sql.NullString{String: sessID, Valid: true},
		Kind:        store.LLMKindSummary,
		Model:       "claude-sonnet-4-6",
		PromptHash:  "h1",
		Body:        "SESSION_SUMMARY_BODY",
		CreatedAtMs: time.Now().UnixMilli(),
	})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed summary: %v", err)
	}
	_ = tx.Commit()

	args := `{"session_id":"` + sessID + `"}`
	res := callTool(t, s, "get_summary", args)
	if res.IsError {
		t.Fatalf("unexpected IsError: %+v", res)
	}
	if res.Content[0].Text != "SESSION_SUMMARY_BODY" {
		t.Errorf("body mismatch: got %q", res.Content[0].Text)
	}
}

func TestGetSummary_MissingSessionIDIsUserError(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "get_summary", `{}`)
	if !res.IsError {
		t.Errorf("expected IsError=true for missing session_id")
	}
}

func TestGetSummary_NoOutputIsUserError(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	// A real session that exists but has no stored LLM output yet —
	// the seeded "sess-foo" has events, no summary row.
	sessID := ingest.DeriveSessionID("claude-code", "sess-foo")
	res := callTool(t, s, "get_summary", `{"session_id":"`+sessID+`"}`)
	if !res.IsError {
		t.Errorf("expected IsError=true for session with no outputs")
	}
	if !strings.Contains(res.Content[0].Text, "no summary output") {
		t.Errorf("expected diagnostic text: %s", res.Content[0].Text)
	}
}

func TestGetSummary_UnknownSessionIsUserError(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	// A syntactically valid session id that doesn't exist in the
	// store resolves through the prefix path and comes back as
	// "no such session" rather than an empty output.
	res := callTool(t, s, "get_summary",
		`{"session_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}`)
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown session")
	}
	if !strings.Contains(res.Content[0].Text, "no such session") {
		t.Errorf("expected 'no such session' diagnostic, got: %s", res.Content[0].Text)
	}
}

func TestGetSummary_AcceptsPrefix(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	// Stash a summary for sess-foo so the happy-path resolves.
	tx, _ := st.DB().Begin()
	sessID := ingest.DeriveSessionID("claude-code", "sess-foo")
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   sql.NullString{String: sessID, Valid: true},
		Kind:        store.LLMKindSummary,
		Model:       "test",
		PromptHash:  "h-prefix",
		Body:        "summary body",
		CreatedAtMs: 1,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed summary: %v", err)
	}
	_ = tx.Commit()

	// Pass only the 8-char preview — the resolver must expand it.
	res := callTool(t, s, "get_summary", `{"session_id":"`+sessID[:8]+`"}`)
	if res.IsError {
		t.Fatalf("prefix should resolve, got error: %s", res.Content[0].Text)
	}
	if res.Content[0].Text != "summary body" {
		t.Errorf("body: got %q, want %q", res.Content[0].Text, "summary body")
	}
}

func TestToolsCall_ScrubsEgressText(t *testing.T) {
	t.Parallel()
	// Plant a secret directly in the store (bypassing the ingest
	// redact, like a pre-redactor store). The egress scrub at the
	// tools/call boundary MUST rewrite it to a marker before it
	// reaches the client.
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	s.RegisterTool(Tool{
		Name:        "leaky",
		Description: "returns a secret on purpose",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, _ json.RawMessage) (*ToolResult, *Error) {
			return TextResult("the leak is AKIAIOSFODNN7EXAMPLE right there"), nil
		},
	})

	// Exercise via the JSON-RPC dispatcher so the scrub in
	// handleToolsCall actually runs.
	in, inW := io.Pipe()
	out := &bytes.Buffer{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = s.Run(context.Background(), in, out) }()

	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"leaky","arguments":{}}}`+"\n")
	_ = inW.Close()
	wg.Wait()

	if strings.Contains(out.String(), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("egress leaked secret:\n%s", out.String())
	}
	// JSON encoder HTML-escapes < and > so the marker reads as
	// <redacted:aws_access_key> on the wire. Check for
	// the pattern name either way.
	if !strings.Contains(out.String(), "redacted:aws_access_key") {
		t.Errorf("expected redaction marker:\n%s", out.String())
	}
}

func TestToolsList_IncludesInputSchema(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	in, inW := io.Pipe()
	out := &bytes.Buffer{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = s.Run(context.Background(), in, out) }()

	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	_ = inW.Close()
	wg.Wait()

	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 3 {
		t.Errorf("expected 3 tools, got %d", len(tools))
	}
	for _, t0 := range tools {
		tool := t0.(map[string]any)
		if _, ok := tool["inputSchema"]; !ok {
			t.Errorf("tool %v missing inputSchema", tool["name"])
		}
	}
}
