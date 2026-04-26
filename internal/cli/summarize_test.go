package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// --- fake LLM client ---

type fakeLLM struct {
	// reply is the convenience knob: when set and the request forces
	// a tool, we synthesize a minimal valid tool_use input keyed off
	// this string (summary.Topic, reflection.WorkflowChange,
	// proposal.Skills[0].WhenToUse all carry `reply`). For legacy
	// text-only paths (ForceTool=="") reply goes into Response.Text.
	reply string
	// toolInput, when non-nil, is returned verbatim as the forced
	// tool_use input. Use this when a test needs a specific schema-
	// compliant shape (e.g. asserting link annotations round-trip).
	toolInput json.RawMessage

	err     error
	called  int
	lastReq llm.Request
}

func (f *fakeLLM) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.called++
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	resp := &llm.Response{
		Model: "claude-sonnet-4-6",
		Usage: llm.Usage{InputTokens: 17, OutputTokens: 23},
	}
	if req.ForceTool == "" {
		resp.Text = f.reply
		return resp, nil
	}
	input := f.toolInput
	if input == nil {
		input = synthMinimalToolInput(req.ForceTool, f.reply)
	}
	resp.ToolUses = []llm.ToolUse{{
		ID:    "toolu_fake",
		Name:  req.ForceTool,
		Input: input,
	}}
	return resp, nil
}

// synthMinimalToolInput returns a schema-compliant tool_use input for
// the named tool, threading `hint` through a prominent field so
// substring-based test assertions still find it in the rendered
// output.
func synthMinimalToolInput(toolName, hint string) json.RawMessage {
	if hint == "" {
		hint = "placeholder"
	}
	switch toolName {
	case prompts.ToolNameSummary:
		b, _ := json.Marshal(prompts.SummaryResult{
			Topic:       hint,
			WhatWasDone: []string{hint},
			Unresolved:  []string{},
			KeyFiles:    []string{},
			Links:       []prompts.LinkAnnotation{},
		})
		return b
	case prompts.ToolNameReflection:
		b, _ := json.Marshal(prompts.ReflectionResult{
			TaskTypes:      []prompts.Evidenced{},
			Frictions:      []prompts.Evidenced{},
			WorkflowChange: hint,
		})
		return b
	case prompts.ToolNameProposal:
		b, _ := json.Marshal(prompts.ProposalResult{
			Skills: []prompts.ProposedSkill{{
				Name:      "stub",
				WhenToUse: hint,
				Why:       hint,
				Evidence: []prompts.ProposalEvidence{
					{SessionID: "s1", Quote: "q", WhatHappened: "h"},
					{SessionID: "s2", Quote: "q", WhatHappened: "h"},
				},
				Frequency: 2,
				Effort:    "small",
			}},
			ClaudeMdEntries: []prompts.ProposedClaudeMdRule{},
			Scripts:         []prompts.ProposedScript{},
		})
		return b
	}
	return json.RawMessage(`{}`)
}

// seedSessionForSummarize writes a short but realistic session. The
// returned sessionID is what `summarize --session` would pass in.
func seedSessionForSummarize(t *testing.T) (*store.Store, string) {
	t.Helper()
	s := testStore(t)
	now := time.Now().UTC()

	fixtures := []struct {
		kind    string
		content string
		off     int
	}{
		{"user_prompt", "how does systemd socket activation work?", 0},
		{"assistant_message", "LISTEN_FDS env + fd 3, see sd_listen_fds(3)", 1},
		{"user_prompt", "show me a Go example", 2},
		{"assistant_message", "net.FileListener(os.NewFile(3, ...))", 3},
	}
	for _, fx := range fixtures {
		env := ingest.Envelope{
			V:               1,
			EventID:         uuid.Must(uuid.NewV7()).String(),
			SourceAgent:     "claude-code",
			SourceSessionID: "sess-summary",
			Kind:            fx.kind,
			Role:            "user",
			TsSource:        now.Add(time.Duration(fx.off) * time.Second),
			Cwd:             "/work/systemd",
			ContentText:     fx.content,
			Payload:         map[string]any{"i": fx.off},
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
	return s, ingest.DeriveSessionID("claude-code", "sess-summary")
}

func TestRunSummarize_HappyPathCallsLLMAndPersists(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)

	f := &fakeLLM{reply: "Topic: socket activation\n..."}
	var out bytes.Buffer

	id, err := RunSummarize(
		context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		SummarizeOptions{SessionID: sessID},
		&out,
	)
	if err != nil {
		t.Fatalf("RunSummarize: %v", err)
	}
	if id == 0 {
		t.Error("expected a non-zero row id")
	}
	if f.called != 1 {
		t.Errorf("LLM call count: got %d, want 1", f.called)
	}
	if !strings.Contains(out.String(), "socket activation") {
		t.Errorf("stdout missing reply:\n%s", out.String())
	}

	// Persisted row must have the full body and the session link.
	var body string
	var sidFromDB string
	_ = s.DB().QueryRow(
		`SELECT body, session_id FROM llm_outputs WHERE id = ?`, id,
	).Scan(&body, &sidFromDB)
	if !strings.Contains(body, "Topic: socket activation") {
		t.Errorf("persisted body: %q", body)
	}
	if sidFromDB != sessID {
		t.Errorf("session_id: got %q, want %q", sidFromDB, sessID)
	}
}

func TestRunSummarize_CacheHitSkipsLLM(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)

	f := &fakeLLM{reply: "cached body X"}
	newClient := func() (llm.Client, error) { return f, nil }

	// First call: populates the cache.
	if _, err := RunSummarize(context.Background(), s, newClient,
		SummarizeOptions{SessionID: sessID}, &bytes.Buffer{}); err != nil {
		t.Fatalf("first: %v", err)
	}

	// Second call with the same input: must NOT call the LLM.
	var out bytes.Buffer
	if _, err := RunSummarize(context.Background(), s, newClient,
		SummarizeOptions{SessionID: sessID}, &out); err != nil {
		t.Fatalf("second: %v", err)
	}
	if f.called != 1 {
		t.Errorf("second call hit LLM; expected cache. calls=%d", f.called)
	}
	if !strings.Contains(out.String(), "cached body X") {
		t.Errorf("stdout should replay cached body:\n%s", out.String())
	}
}

func TestRunSummarize_ForceBypassesCache(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	f := &fakeLLM{reply: "first body"}
	newClient := func() (llm.Client, error) { return f, nil }

	if _, err := RunSummarize(context.Background(), s, newClient,
		SummarizeOptions{SessionID: sessID}, &bytes.Buffer{}); err != nil {
		t.Fatalf("first: %v", err)
	}

	// --force should re-call. Change the reply to prove it ran.
	f.reply = "second body"
	var out bytes.Buffer
	if _, err := RunSummarize(context.Background(), s, newClient,
		SummarizeOptions{SessionID: sessID, Force: true}, &out); err != nil {
		t.Fatalf("second: %v", err)
	}
	if f.called != 2 {
		t.Errorf("expected 2 LLM calls under --force, got %d", f.called)
	}
	// The first body must still be in the DB — the cache is
	// idempotent, --force just adds a call, it doesn't overwrite.
	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM llm_outputs WHERE session_id = ?`, sessID).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 row (dedup on prompt_hash), got %d", n)
	}
}

func TestRunSummarize_CacheHitDoesNotRequireAPIKey(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)

	// Populate cache with a working fake client.
	goodClient := &fakeLLM{reply: "warm"}
	if _, err := RunSummarize(context.Background(), s,
		func() (llm.Client, error) { return goodClient, nil },
		SummarizeOptions{SessionID: sessID}, &bytes.Buffer{}); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// Second call uses a constructor that would error — but shouldn't
	// be invoked because the cache hit comes first.
	var out bytes.Buffer
	if _, err := RunSummarize(context.Background(), s,
		func() (llm.Client, error) { return nil, errors.New("no api key") },
		SummarizeOptions{SessionID: sessID}, &out); err != nil {
		t.Fatalf("cache-hit path invoked the API-key-requiring constructor: %v", err)
	}
	if !strings.Contains(out.String(), "warm") {
		t.Errorf("expected cached body replayed:\n%s", out.String())
	}
}

func TestRunSummarize_UnknownSessionIsError(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	// A syntactically valid UUID that doesn't exist in the store.
	// The prefix resolver rejects it before we ever try to load
	// events, so the diagnostic is "no such session", not
	// "no events" — more useful for the user.
	_, err := RunSummarize(context.Background(), s,
		func() (llm.Client, error) { return &fakeLLM{}, nil },
		SummarizeOptions{SessionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "no such session") {
		t.Fatalf("expected 'no such session' error, got %v", err)
	}
}

func TestRunSummarize_AcceptsSessionPrefix(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	var out bytes.Buffer
	f := &fakeLLM{reply: "summary via prefix"}

	// Pass only the 8-char preview — the resolver must expand it
	// to the full id before the loader runs.
	if _, err := RunSummarize(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		SummarizeOptions{SessionID: sessID[:8]}, &out); err != nil {
		t.Fatalf("prefix should resolve: %v", err)
	}
	if !strings.Contains(out.String(), "summary via prefix") {
		t.Errorf("output: %q", out.String())
	}
}

func TestRunSummarize_PersistsJSONBodyAndRendersTopic(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)

	// Precise tool payload with links so we can assert both the
	// stored JSON shape and the rendered output.
	input := prompts.SummaryResult{
		Topic:       "stored-json-topic",
		WhatWasDone: []string{"did a thing", "did another"},
		Unresolved:  []string{"still broken"},
		KeyFiles:    []string{"file/a.go"},
		Links: []prompts.LinkAnnotation{
			{URL: "https://example.com/a", UsedFor: "reading the spec"},
		},
	}
	raw, _ := json.Marshal(input)
	f := &fakeLLM{toolInput: raw}

	var out bytes.Buffer
	id, err := RunSummarize(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		SummarizeOptions{SessionID: sessID}, &out)
	if err != nil {
		t.Fatalf("RunSummarize: %v", err)
	}

	// Rendered output — human-readable bits.
	rendered := out.String()
	for _, want := range []string{
		"stored-json-topic",
		"did a thing",
		"still broken",
		"file/a.go",
		"https://example.com/a",
		"reading the spec",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered output missing %q:\n%s", want, rendered)
		}
	}

	// Stored body — JSON, round-trips.
	var body string
	_ = s.DB().QueryRow(`SELECT body FROM llm_outputs WHERE id = ?`, id).Scan(&body)
	var back prompts.SummaryResult
	if err := json.Unmarshal([]byte(body), &back); err != nil {
		t.Fatalf("stored body is not valid JSON: %v\nbody=%s", err, body)
	}
	if back.Topic != input.Topic {
		t.Errorf("round-trip topic: got %q, want %q", back.Topic, input.Topic)
	}
	if len(back.Links) != 1 || back.Links[0].URL != "https://example.com/a" {
		t.Errorf("round-trip links: got %+v", back.Links)
	}
}

func TestRunSummarize_JSONFlagEmitsRawBody(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	f := &fakeLLM{reply: "t"}

	var out bytes.Buffer
	if _, err := RunSummarize(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		SummarizeOptions{SessionID: sessID, JSON: true}, &out); err != nil {
		t.Fatalf("RunSummarize: %v", err)
	}
	// With --json, output must be valid JSON matching SummaryResult.
	var parsed prompts.SummaryResult
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("--json output is not valid SummaryResult JSON: %v\n%s", err, out.String())
	}
	if parsed.Topic != "t" {
		t.Errorf("topic: got %q, want %q", parsed.Topic, "t")
	}
}

func TestRunSummarize_ModelRefusesToolIsClearError(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)

	// Force the fake to return text only (no tool_use) even though
	// the request forced record_summary. parseToolResult must turn
	// this into a user-visible error, not a silent pass-through.
	textOnly := &textOnlyFakeLLM{text: "I refuse"}

	_, err := RunSummarize(context.Background(), s,
		func() (llm.Client, error) { return textOnly, nil },
		SummarizeOptions{SessionID: sessID}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error when model ignores tool_choice")
	}
	if !strings.Contains(err.Error(), "did not call") {
		t.Errorf("expected diagnostic about missing tool call, got %v", err)
	}
}

// textOnlyFakeLLM returns only Text, ignoring Request.Tools. Used to
// prove the CLI wrappers surface a clear error when the model fails
// to honor tool_choice.
type textOnlyFakeLLM struct{ text string }

func (t *textOnlyFakeLLM) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return &llm.Response{Text: t.text, Model: "test"}, nil
}

func TestRunSummarize_LoadsAndPassesLinksToPromptBuilder(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)

	// Seed URL extractions on the session so the wiring from store
	// -> prompts.BuildSummary -> Request.Messages[0].Content carries
	// them through.
	var eventID string
	_ = s.DB().QueryRow(`SELECT event_id FROM events WHERE session_id = ? LIMIT 1`, sessID).Scan(&eventID)
	for _, u := range []string{"https://from-extractions.example/x", "https://from-extractions.example/y"} {
		if _, err := s.DB().Exec(
			`INSERT INTO extractions(event_id, session_id, kind, value) VALUES (?, ?, 'url', ?)`,
			eventID, sessID, u,
		); err != nil {
			t.Fatalf("seed extraction: %v", err)
		}
	}

	f := &fakeLLM{reply: "t"}
	if _, err := RunSummarize(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		SummarizeOptions{SessionID: sessID}, &bytes.Buffer{}); err != nil {
		t.Fatalf("RunSummarize: %v", err)
	}
	// The fake captured the Request; confirm the user message carries
	// both URLs in the Links stanza.
	if len(f.lastReq.Messages) == 0 {
		t.Fatal("no request captured")
	}
	body := f.lastReq.Messages[0].Content
	if !strings.Contains(body, "Links observed in this session") {
		t.Errorf("links stanza missing:\n%s", body)
	}
	for _, u := range []string{"from-extractions.example/x", "from-extractions.example/y"} {
		if !strings.Contains(body, u) {
			t.Errorf("url %q not routed into prompt:\n%s", u, body)
		}
	}
}

func TestRunSummarize_LLMErrorSurfaces(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	f := &fakeLLM{err: errors.New("boom")}
	_, err := RunSummarize(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		SummarizeOptions{SessionID: sessID},
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected wrapped LLM error, got %v", err)
	}

	// Failure must NOT leave a partial row in llm_outputs.
	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM llm_outputs`).Scan(&n)
	if n != 0 {
		t.Errorf("expected 0 rows after error, got %d", n)
	}
}

func TestRunSummarize_ModelOverrideFlowsThroughRequest(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	f := &fakeLLM{reply: "ok"}
	if _, err := RunSummarize(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		SummarizeOptions{SessionID: sessID, Model: "claude-opus-4-7"},
		&bytes.Buffer{},
	); err != nil {
		t.Fatalf("RunSummarize: %v", err)
	}
	if f.lastReq.Model != "claude-opus-4-7" {
		t.Errorf("Model override: got %q", f.lastReq.Model)
	}
}
