package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/events"
)

// seedSearchableEvent ingests one user_prompt with the given
// content_text so FTS finds it. Returns the derived session id so
// the test can assert it appears in the summary.
func seedSearchableEvent(t *testing.T, s *store.Store, sessionKey, prompt string) string {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sessionKey,
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Now().UTC().Add(-time.Hour),
		ContentText:     prompt,
		Cwd:             "/work",
		Payload:         map[string]any{},
		Redaction:       &events.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(env)
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.IngestEnvelope(t.Context(), tx, env, raw, env.TsSource.UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ingest: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return events.DeriveSessionID("claude-code", sessionKey)
}

func TestRunSearchSummary_NoHitsPrintsEmptyMarker(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)

	var buf bytes.Buffer
	called := 0
	newClient := func() (llm.Client, error) {
		called++
		return &fakeLLM{reply: "should not be called"}, nil
	}
	err := RunSearchSummary(context.Background(), apiForStore(t, s),
		SearchOptions{Query: "noresults", TopN: 5},
		newClient, &buf)
	if err != nil {
		t.Fatalf("RunSearchSummary: %v", err)
	}
	if !strings.Contains(buf.String(), "(no hits") {
		t.Errorf("want empty-marker, got %q", buf.String())
	}
	if called != 0 {
		t.Errorf("LLM called on empty hits: count=%d", called)
	}
}

// TestRunSearchSummary_PassesGroundedHitsToLLM is the core
// regression: hit content + session_id end up in the LLM prompt
// so the synthesised answer can cite them.
func TestRunSearchSummary_PassesGroundedHitsToLLM(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	seedSearchableEvent(t, s, "summary-sess-1", "investigate slow query plan")

	f := &fakeLLM{reply: "Slow queries traced to a missing index [session=" +
		events.DeriveSessionID("claude-code", "summary-sess-1")[:8] + "]."}

	var buf bytes.Buffer
	err := RunSearchSummary(context.Background(), apiForStore(t, s),
		SearchOptions{Query: "slow query", TopN: 5},
		func() (llm.Client, error) { return f, nil }, &buf)
	if err != nil {
		t.Fatalf("RunSearchSummary: %v", err)
	}
	if f.called != 1 {
		t.Fatalf("LLM call count: got %d, want 1", f.called)
	}

	prompt := f.lastReq.Messages[0].Content
	for _, want := range []string{"Query: slow query", "investigate slow query plan", "[session="} {
		if !strings.Contains(prompt, want) {
			t.Errorf("LLM prompt missing %q\n%s", want, prompt)
		}
	}
	if !strings.Contains(buf.String(), "Slow queries traced") {
		t.Errorf("want LLM reply on stdout, got %q", buf.String())
	}
}

// TestRunSearchSummary_JSONIncludesSummaryAndHits pins the JSON
// shape: callers (web, MCP) get both the synthesised answer and
// the hits it was grounded in for click-through.
func TestRunSearchSummary_JSONIncludesSummaryAndHits(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	seedSearchableEvent(t, s, "summary-sess-2", "FTS5 trigram tokenizer notes")

	var buf bytes.Buffer
	err := RunSearchSummary(context.Background(), apiForStore(t, s),
		SearchOptions{Query: "FTS5", TopN: 5, Format: FormatJSON},
		func() (llm.Client, error) { return &fakeLLM{reply: "synthesised answer"}, nil },
		&buf)
	if err != nil {
		t.Fatalf("RunSearchSummary: %v", err)
	}
	var got struct {
		Query   string          `json:"query"`
		Summary string          `json:"summary"`
		Hits    []SearchHitJSON `json:"hits"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, buf.String())
	}
	if got.Summary != "synthesised answer" {
		t.Errorf("summary: %q", got.Summary)
	}
	if len(got.Hits) == 0 {
		t.Error("expected at least one hit in payload")
	}
	if got.Query != "FTS5" {
		t.Errorf("query echo: %q", got.Query)
	}
}

func TestRunSearchSummary_ClampsTopNToFive(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	for i := 0; i < 12; i++ {
		seedSearchableEvent(t, s, "topn-sess", "needle "+string(rune('a'+i)))
	}
	f := &fakeLLM{reply: "ok"}
	var buf bytes.Buffer
	err := RunSearchSummary(context.Background(), apiForStore(t, s),
		SearchOptions{Query: "needle", TopN: 0}, // 0 → default 5
		func() (llm.Client, error) { return f, nil }, &buf)
	if err != nil {
		t.Fatalf("RunSearchSummary: %v", err)
	}
	hitsBlock := f.lastReq.Messages[0].Content
	count := strings.Count(hitsBlock, "[session=")
	if count > 5 {
		t.Errorf("default top_n: got %d hits in prompt, want <=5", count)
	}
}
