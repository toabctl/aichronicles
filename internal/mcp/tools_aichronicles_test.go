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
	"github.com/toabctl/aichronicles/pkg/redact"
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

func TestRegisterAichroniclesTools_InstallsAllFive(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	for _, want := range []string{
		"search_events", "list_sessions", "find_episodes", "get_summary",
		"list_subagents", "get_unresolved_for_cwd", "list_workflows",
		"get_facts_for_subject", "find_fact_subjects",
		"get_project_context",
	} {
		if _, ok := s.tools[want]; !ok {
			t.Errorf("tool %q not registered", want)
		}
	}
}

// seedWorkflowOutput inserts one llm_outputs row of kind=induction
// carrying the supplied workflow shape inside body.workflow. Round 8
// merged workflow extraction into the unified record_induction call,
// so workflows now ride along inside induction rows; tests that
// previously seeded kind=workflow rows seed kind=induction with a
// non-null workflow field.
//
// `found` is the legacy-named flag preserved for test readability;
// when true the induction body carries body.workflow; when false it
// carries body.workflow=null and body.rationale=<rationale>.
func seedWorkflowOutput(t *testing.T, st *store.Store, sessionID, taskShape, rationale string, found bool) int64 {
	t.Helper()
	bodyMap := map[string]any{
		"rationale": rationale,
	}
	if found {
		bodyMap["workflow"] = map[string]any{
			"task_shape": taskShape,
			"procedure": []any{
				// `{arg}` is declared so the unmarshal-time AWM
				// consistency check accepts the fixture.
				map[string]any{
					"action": "Do step one with {arg}",
					"placeholders": []any{
						map[string]any{"token": "arg", "description": "step argument", "example": "value"},
					},
				},
				map[string]any{"action": "Do step two"},
			},
			"preconditions":  []string{"git working tree clean"},
			"success_checks": []string{"all tests pass"},
			"evidence": []any{
				map[string]any{
					"session_id":    sessionID,
					"quote":         "x",
					"what_happened": "y",
				},
			},
		}
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, 'claude-code', ?)
		 ON CONFLICT(id) DO NOTHING`,
		sessionID, "src-"+sessionID,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	r, err := st.DB().Exec(
		`INSERT INTO llm_outputs(session_id, kind, model, prompt_hash, body, created_at_ms)
		 VALUES (?, 'induction', 'fake-model', ?, ?, ?)`,
		sessionID, "h-"+t.Name()+"-"+sessionID, string(body), time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("insert induction: %v", err)
	}
	id, err := r.LastInsertId()
	if err != nil {
		t.Fatalf("last id: %v", err)
	}
	return id
}

// seedFactsRow inserts an llm_outputs row of kind=facts (the FK
// target for semantic_facts.source_llm_output_id) and returns its id.
func seedFactsRow(t *testing.T, st *store.Store) int64 {
	t.Helper()
	r, err := st.DB().Exec(
		`INSERT INTO llm_outputs(kind, model, prompt_hash, body, created_at_ms)
		 VALUES ('facts', 'fake-model', ?, '{}', ?)`,
		"h-"+t.Name(), time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("seed llm_output: %v", err)
	}
	id, err := r.LastInsertId()
	if err != nil {
		t.Fatalf("last id: %v", err)
	}
	return id
}

func TestGetFactsForSubject_RendersFacts(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	loID := seedFactsRow(t, st)

	for _, f := range []store.SemanticFact{
		{
			SourceLLMOutputID: loID,
			Subject:           "/work/aichronicles",
			Predicate:         "uses_language_version",
			Object:            "Go 1.26",
			Confidence:        0.95,
			AssertedAtMs:      time.Now().UnixMilli(),
		},
		{
			SourceLLMOutputID: loID,
			Subject:           "/work/aichronicles",
			Predicate:         "runs_tests_via",
			Object:            "go test ./...",
			Confidence:        0.9,
			AssertedAtMs:      time.Now().UnixMilli(),
		},
	} {
		if _, err := store.SaveSemanticFact(t.Context(), st.DB(), f); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "get_facts_for_subject", `{"subject":"/work/aichronicles"}`)
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty result")
	}
	body := res.Content[0].Text
	for _, want := range []string{
		"subject: /work/aichronicles",
		"uses_language_version",
		"Go 1.26",
		"runs_tests_via",
		"go test ./...",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

func TestGetFactsForSubject_EmptyResultIsHelpful(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "get_facts_for_subject", `{"subject":"/no-such-project"}`)
	body := res.Content[0].Text
	if !strings.Contains(body, "no facts known") {
		t.Errorf("expected helpful empty-state message, got:\n%s", body)
	}
}

func TestGetFactsForSubject_RejectsEmptySubject(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "get_facts_for_subject", `{"subject":"  "}`)
	if !strings.Contains(res.Content[0].Text, "subject is required") {
		t.Errorf("expected validation error, got %+v", res)
	}
}

func TestFindFactSubjects_CaseInsensitiveSubstring(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	loID := seedFactsRow(t, st)
	for _, sub := range []string{
		"/work/aichronicles",
		"/work/Aichronicles-fork",
		"/work/systemd",
	} {
		if _, err := store.SaveSemanticFact(t.Context(), st.DB(), store.SemanticFact{
			SourceLLMOutputID: loID,
			Subject:           sub,
			Predicate:         "primary_language",
			Object:            "Go",
			Confidence:        1.0,
			AssertedAtMs:      time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("save %s: %v", sub, err)
		}
	}

	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "find_fact_subjects", `{"contains":"aichronicles"}`)
	body := res.Content[0].Text
	if !strings.Contains(body, "/work/aichronicles") || !strings.Contains(body, "/work/Aichronicles-fork") {
		t.Errorf("expected case-insensitive match, body:\n%s", body)
	}
	if strings.Contains(body, "/work/systemd") {
		t.Errorf("expected non-matching subject filtered out, body:\n%s", body)
	}
}

func TestFindFactSubjects_RejectsEmptyNeedle(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "find_fact_subjects", `{"contains":"  "}`)
	if !strings.Contains(res.Content[0].Text, "contains is required") {
		t.Errorf("expected validation error, got %+v", res)
	}
}

func TestFindFactSubjects_NoMatchesIsHelpful(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "find_fact_subjects", `{"contains":"absolutely-nothing"}`)
	if !strings.Contains(res.Content[0].Text, "no fact subjects matched") {
		t.Errorf("expected empty-state message, got %+v", res)
	}
}

func TestListWorkflows_ReturnsFoundWorkflowsWithProcedure(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	seedWorkflowOutput(t, st, "00000000-0000-0000-0000-0000000000aa",
		"deploy a backend service to staging", "extracted from session", true)
	seedWorkflowOutput(t, st, "00000000-0000-0000-0000-0000000000bb",
		"investigate a failing CI run", "extracted from session", true)

	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "list_workflows", `{}`)
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty result")
	}
	body := res.Content[0].Text
	for _, want := range []string{
		"deploy a backend service to staging",
		"investigate a failing CI run",
		"1. Do step one with {arg}",
		"2. Do step two",
		"preconditions:",
		"git working tree clean",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

func TestListWorkflows_FiltersByTaskShapeContains(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	seedWorkflowOutput(t, st, "00000000-0000-0000-0000-0000000000cc",
		"deploy a backend service to staging", "x", true)
	seedWorkflowOutput(t, st, "00000000-0000-0000-0000-0000000000dd",
		"investigate a failing CI run", "x", true)

	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "list_workflows", `{"task_shape_contains":"deploy"}`)
	body := res.Content[0].Text
	if !strings.Contains(body, "deploy a backend service to staging") {
		t.Errorf("expected deploy match, got:\n%s", body)
	}
	if strings.Contains(body, "investigate a failing CI") {
		t.Errorf("expected non-matching workflow filtered out, got:\n%s", body)
	}
}

func TestListWorkflows_DefaultsExcludeNotFound(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	seedWorkflowOutput(t, st, "00000000-0000-0000-0000-0000000000ee",
		"", "session was a one-off bug fix", false) // found=false
	seedWorkflowOutput(t, st, "00000000-0000-0000-0000-0000000000ff",
		"deploy something", "extracted", true)

	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	// Default omits found=false rows.
	res := callTool(t, s, "list_workflows", `{}`)
	body := res.Content[0].Text
	if strings.Contains(body, "no workflow") || strings.Contains(body, "one-off bug fix") {
		t.Errorf("default should exclude found=false rows, body:\n%s", body)
	}
	if !strings.Contains(body, "deploy something") {
		t.Errorf("expected found=true row to appear, body:\n%s", body)
	}

	// Explicit include_not_found surfaces them.
	res2 := callTool(t, s, "list_workflows", `{"include_not_found":true}`)
	body2 := res2.Content[0].Text
	if !strings.Contains(body2, "no workflow") {
		t.Errorf("include_not_found:true should surface no-workflow verdicts, body:\n%s", body2)
	}
}

func TestListWorkflows_EmptyResultIsHelpful(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "list_workflows", `{}`)
	body := res.Content[0].Text
	if !strings.Contains(body, "no workflows yet") {
		t.Errorf("expected helpful empty-state message, got:\n%s", body)
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

// TestToolsCall_PassesIngestRedactedContentThrough encodes the
// new redaction contract: ingest is the single point of truth.
// Drive a real envelope (carrying a planted secret) through the
// edge redactor + IngestEnvelope, then call search_events via
// the JSON-RPC dispatcher and assert (a) the raw secret never
// reaches the wire because ingest already replaced it, and (b)
// the resulting <redacted:...> marker survives untouched — the
// dispatcher does NOT re-scrub on the read path.
func TestToolsCall_PassesIngestRedactedContentThrough(t *testing.T) {
	t.Parallel()

	st := openSeededStore(t)

	// Ingest one user_prompt that contains a secret. The edge
	// redactor inside ApplyRedaction rewrites it to the
	// <redacted:aws_access_key> marker before the row lands in
	// the store.
	const planted = "the leak is AKIAIOSFODNN7EXAMPLE right there"
	env := ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-leak",
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Now().UTC(),
		Cwd:             "/work/leak",
		ContentText:     planted,
		Payload:         map[string]any{"prompt": planted},
	}
	ingest.ApplyRedaction(&env, redact.Default())
	if !env.Redaction.Applied {
		t.Fatalf("ApplyRedaction did not flag the envelope: %+v", env.Redaction)
	}
	raw, _ := json.Marshal(&env)
	tx, _ := st.DB().Begin()
	if _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("IngestEnvelope: %v", err)
	}
	_ = tx.Commit()

	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	// Exercise via the JSON-RPC dispatcher — search_events would
	// route every byte through the old egress wrapper if it still
	// existed.
	in, inW := io.Pipe()
	out := &bytes.Buffer{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = s.Run(context.Background(), in, out) }()

	_, _ = io.WriteString(inW,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+
			`{"name":"search_events","arguments":{"query":"leak"}}}`+"\n")
	_ = inW.Close()
	wg.Wait()

	wire := out.String()
	if strings.Contains(wire, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("ingest-redacted secret leaked to MCP wire:\n%s", wire)
	}
	// JSON encoder HTML-escapes < and >; the on-wire form is
	// <redacted:aws_access_key> or escaped HTML.
	// Either way the pattern name is the load-bearing substring.
	if !strings.Contains(wire, "redacted:aws_access_key") {
		t.Errorf("expected ingest-installed redaction marker on the wire:\n%s", wire)
	}
}

// TestToolsCall_PassesUnredactedContentThrough makes the new
// contract bite both ways: when a tool registers a handler that
// returns a raw secret (bypassing the store entirely — like a
// future custom tool would), the dispatcher does not protect
// against that. Ingress is the single point of truth.
//
// Why we test this: we deliberately removed the read-path
// safety net. The test pins the decision so a future "let's
// just add it back, what could go wrong" PR has to delete this
// test (and someone reviews why).
func TestToolsCall_PassesUnredactedContentThrough(t *testing.T) {
	t.Parallel()
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	s.RegisterTool(Tool{
		Name:        "leaky",
		Description: "returns a secret on purpose",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, _ json.RawMessage) (*ToolResult, *Error) {
			return TextResult("the leak is AKIAIOSFODNN7EXAMPLE right there"), nil
		},
	})

	in, inW := io.Pipe()
	out := &bytes.Buffer{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = s.Run(context.Background(), in, out) }()

	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"leaky","arguments":{}}}`+"\n")
	_ = inW.Close()
	wg.Wait()

	if !strings.Contains(out.String(), "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("dispatcher should pass tool content verbatim — read path no longer scrubs:\n%s", out.String())
	}
}

// seedSubagentEvents drops a top-level event plus two events
// from the same sub-agent thread into a fresh store. Returns the
// store and the subagent_id so callers can drive the relevant
// tools.
func seedSubagentEvents(t *testing.T) (*store.Store, string) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ingestOne := func(sa *ingest.Subagent, content string, tsOffset time.Duration) {
		t.Helper()
		env := ingest.Envelope{
			V:               1,
			EventID:         uuid.Must(uuid.NewV7()).String(),
			SourceAgent:     "claude-code",
			SourceSessionID: "sess-sa",
			Kind:            "user_prompt",
			Role:            "user",
			TsSource:        time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC).Add(tsOffset),
			Cwd:             "/work/sess-sa",
			ContentText:     content,
			Payload:         map[string]any{},
			Subagent:        sa,
			Redaction:       &ingest.Redaction{Applied: true},
		}
		raw, _ := json.Marshal(&env)
		tx, _ := s.DB().Begin()
		if _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			t.Fatalf("seed: %v", err)
		}
		_ = tx.Commit()
	}

	const subagentID = "agent-7"
	ingestOne(nil, "main agent prompt", 0)
	ingestOne(&ingest.Subagent{ID: subagentID, Type: "planner"}, "planner step one", time.Second)
	ingestOne(&ingest.Subagent{ID: subagentID, Type: "planner"}, "planner step two", 2*time.Second)
	return s, subagentID
}

func TestListSubagents_AggregatesSpan(t *testing.T) {
	t.Parallel()
	st, subID := seedSubagentEvents(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "list_subagents", `{}`)
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	out := res.Content[0].Text
	if !strings.Contains(out, subID) {
		t.Errorf("expected subagent_id %q in output:\n%s", subID, out)
	}
	if !strings.Contains(out, "planner") {
		t.Errorf("expected subagent_type label in output:\n%s", out)
	}
	// Two events for that thread; output should mention 2 in the
	// event_count column. Crude substring check is enough.
	if !strings.Contains(out, "\t2\n") {
		t.Errorf("expected event_count=2 in output:\n%s", out)
	}
}

func TestListSubagents_EmptyStore(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "list_subagents", `{}`)
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	if res.Content[0].Text != "(no subagent threads)" {
		t.Errorf("unexpected output: %q", res.Content[0].Text)
	}
}

// TestSearchEvents_UnknownSubagentIDIsClearError pins the B6
// audit fix: an unknown subagent_id must surface as an explicit
// error, not silently return zero hits (which is
// indistinguishable from "real subagent with no matches" and
// hides typos from the calling agent).
func TestSearchEvents_UnknownSubagentIDIsClearError(t *testing.T) {
	t.Parallel()
	st, _ := seedSubagentEvents(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "search_events",
		`{"query":"step","subagent_id":"agent-does-not-exist"}`)
	if !res.IsError {
		t.Fatalf("expected IsError=true for unknown subagent_id, got %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "no events for subagent_id") {
		t.Errorf("expected diagnostic, got: %s", res.Content[0].Text)
	}
}

func TestSearchEvents_SubagentIDFilterNarrows(t *testing.T) {
	t.Parallel()
	st, subID := seedSubagentEvents(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	// Without filter: matches the main-agent and the two
	// subagent rows when query is broad enough.
	resAll := callTool(t, s, "search_events", `{"query":"prompt"}`)
	if !strings.Contains(resAll.Content[0].Text, "main agent") {
		t.Errorf("unfiltered search should still see top-level rows:\n%s", resAll.Content[0].Text)
	}

	// With subagent_id filter: top-level row must be excluded.
	res := callTool(t, s, "search_events",
		`{"query":"step","subagent_id":"`+subID+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	out := res.Content[0].Text
	if strings.Contains(out, "main agent") {
		t.Errorf("subagent_id filter should exclude top-level rows:\n%s", out)
	}
	if !strings.Contains(out, "planner step") {
		t.Errorf("expected subagent rows in filtered output:\n%s", out)
	}
}

func TestGetUnresolvedForCwd_RequiresCwd(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "get_unresolved_for_cwd", `{}`)
	if !res.IsError {
		t.Errorf("expected IsError=true when cwd is missing")
	}
}

func TestGetUnresolvedForCwd_EmptyCwdReturnsFriendlyMessage(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "get_unresolved_for_cwd", `{"cwd":"/no/such/dir"}`)
	if res.IsError {
		t.Errorf("unknown-cwd should be a clean empty result, got error: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "no unresolved items") {
		t.Errorf("expected friendly empty message: %s", res.Content[0].Text)
	}
}

func TestGetUnresolvedForCwd_ReturnsItems(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	// The seeded "sess-foo" has cwd /work/sess-foo. Plant a
	// summary with two unresolved items.
	sessID := ingest.DeriveSessionID("claude-code", "sess-foo")
	body := `{"topic":"jsonl parsing","what_was_done":["x"],"unresolved":["land the migration","verify the redaction passthrough"],"key_files":[],"links":[]}`
	tx, _ := st.DB().Begin()
	_, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   sql.NullString{String: sessID, Valid: true},
		Kind:        store.LLMKindSummary,
		Model:       "fake-model",
		PromptHash:  "h-unresolved",
		Body:        body,
		CreatedAtMs: time.Now().UnixMilli(),
	})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed summary: %v", err)
	}
	_ = tx.Commit()

	res := callTool(t, s, "get_unresolved_for_cwd", `{"cwd":"/work/sess-foo"}`)
	if res.IsError {
		t.Fatalf("expected success: %+v", res)
	}
	text := res.Content[0].Text
	for _, want := range []string{
		"jsonl parsing",
		"land the migration",
		"verify the redaction passthrough",
		"/work/sess-foo",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
}

// seedEpisodeForTool inserts one episode row directly so the
// find_episodes tool tests below have predictable data without
// depending on the daemon's induction sweep wiring.
func seedEpisodeForTool(t *testing.T, st *store.Store, sessionID string, ord int, startMs, endMs int64, cwd, intent string) {
	t.Helper()
	if _, err := st.DB().Exec(
		`INSERT OR IGNORE INTO sessions(id, source_agent, source_session_id) VALUES (?, 'claude-code', ?)`,
		sessionID, "src-"+sessionID,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	eid := uuid.Must(uuid.NewV7()).String()
	if _, err := st.DB().Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES (?, ?, ?, ?, ?, ?, '{}')`,
		eid, time.Now().UnixNano()+int64(ord), "claude-code", "src-"+sessionID, startMs, startMs,
	); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, content_text, cwd)
		 VALUES (?, ?, 'claude-code', 'user_prompt', ?, ?, ?)`,
		eid, sessionID, startMs, intent, cwd,
	); err != nil {
		t.Fatalf("event: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO episodes(session_id, ordinal, started_at_ms, ended_at_ms,
			cwd, intent_summary, event_count, first_event_id)
		 VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
		sessionID, ord, startMs, endMs, cwd, intent, eid,
	); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
}

func TestFindEpisodesTool_FiltersAndRendersRows(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	const (
		sessA = "00000000-0000-0000-0000-00000000a1a1"
		sessB = "00000000-0000-0000-0000-00000000b2b2"
	)
	now := time.Now().UnixMilli()
	seedEpisodeForTool(t, st, sessA, 1, now-3600_000, now-3000_000, "/repo/x", "fix the failing build")
	seedEpisodeForTool(t, st, sessB, 1, now-1800_000, now-1200_000, "/repo/y", "explore the new module")

	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	// Bare call returns both rows, newest first.
	all := callTool(t, s, "find_episodes", `{}`)
	if all.IsError {
		t.Fatalf("bare call: %+v", all)
	}
	body := all.Content[0].Text
	if !strings.Contains(body, "fix the failing build") || !strings.Contains(body, "explore the new module") {
		t.Errorf("expected both intents in output, got:\n%s", body)
	}
	// Newest first → sessB's row should appear before sessA's.
	if strings.Index(body, "explore") > strings.Index(body, "fix the failing build") {
		t.Errorf("ordering broken (expected newest first):\n%s", body)
	}

	// query filter narrows to one row.
	q := callTool(t, s, "find_episodes", `{"query":"FAILING"}`)
	if q.IsError {
		t.Fatalf("query call: %+v", q)
	}
	if c := strings.Count(q.Content[0].Text, "\n"); c != 1 {
		t.Errorf("query filter: expected 1 row (one newline), got %d:\n%s", c, q.Content[0].Text)
	}
	if !strings.Contains(q.Content[0].Text, "fix the failing build") {
		t.Errorf("query filter missed the right row:\n%s", q.Content[0].Text)
	}

	// cwd filter narrows to /repo/y (one row).
	cwd := callTool(t, s, "find_episodes", `{"cwd":"/repo/y"}`)
	if cwd.IsError {
		t.Fatalf("cwd call: %+v", cwd)
	}
	if !strings.Contains(cwd.Content[0].Text, "explore the new module") ||
		strings.Contains(cwd.Content[0].Text, "fix the failing build") {
		t.Errorf("cwd filter wrong rows:\n%s", cwd.Content[0].Text)
	}
}

// TestFindEpisodesTool_AcceptsSessionIDPrefix pins the symmetry with
// list_subagents: a model that copies the 8-char id this same tool
// emits back in as session_id must hit the right row, not silently
// get (no episodes) because of a UUID-vs-prefix mismatch.
func TestFindEpisodesTool_AcceptsSessionIDPrefix(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	const sessID = "00000000-0000-0000-0000-00000000aabb"
	now := time.Now().UnixMilli()
	seedEpisodeForTool(t, st, sessID, 1, now-3600_000, now-3000_000, "/repo/x", "prefix-resolved hit")

	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	// Pass the 8-char prefix the tool itself emits.
	res := callTool(t, s, "find_episodes", `{"session_id":"00000000"}`)
	if res.IsError {
		t.Fatalf("prefix call: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "prefix-resolved hit") {
		t.Errorf("prefix should resolve to the seeded session, got:\n%s", res.Content[0].Text)
	}
}

func TestFindEpisodesTool_NoEpisodesReturnsClearMessage(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "find_episodes", `{}`)
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "no episodes") {
		t.Errorf("expected '(no episodes)' message, got:\n%s", res.Content[0].Text)
	}
}

func TestFindEpisodesTool_BadArgsReturnsProtocolError(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	tool := s.tools["find_episodes"]
	_, mcpErr := tool.Handler(context.Background(), json.RawMessage(`{"limit":"oops"}`))
	if mcpErr == nil {
		t.Fatal("expected protocol error for malformed args")
	}
	if mcpErr.Code != InvalidParams {
		t.Errorf("expected InvalidParams (%d), got %d", InvalidParams, mcpErr.Code)
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
	if len(tools) != 10 {
		t.Errorf("expected 10 tools, got %d", len(tools))
	}
	for _, t0 := range tools {
		tool := t0.(map[string]any)
		if _, ok := tool["inputSchema"]; !ok {
			t.Errorf("tool %v missing inputSchema", tool["name"])
		}
	}
}
