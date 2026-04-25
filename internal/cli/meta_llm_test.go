package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
)

// seedSessionsForMeta writes `count` short sessions, each with one
// user_prompt + one assistant_message. Timestamps are spread across
// the last `count` hours so --since windows behave predictably.
func seedSessionsForMeta(t *testing.T, count int) *store.Store {
	t.Helper()
	s := testStore(t)
	now := time.Now().UTC()

	for i := 0; i < count; i++ {
		// Deterministic per-iteration id — don't use the first 8
		// chars of a UUIDv7 here, they're identical in a tight loop
		// because UUIDv7 leads with a millisecond-resolution
		// timestamp.
		sessNative := fmt.Sprintf("sess-meta-%03d", i)
		start := now.Add(-time.Duration(i+1) * time.Hour)

		for off, kind := range []string{"user_prompt", "assistant_message"} {
			env := ingest.Envelope{
				V:               1,
				EventID:         uuid.Must(uuid.NewV7()).String(),
				SourceAgent:     "claude-code",
				SourceSessionID: sessNative,
				Kind:            kind,
				Role:            "user",
				TsSource:        start.Add(time.Duration(off) * time.Second),
				Cwd:             "/work/x",
				ContentText:     "content for " + sessNative + " " + kind,
				Payload:         map[string]any{"i": i, "off": off},
				Redaction:       &ingest.Redaction{Applied: true},
			}
			raw, _ := json.Marshal(env)
			tx, _ := s.DB().Begin()
			if _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
				_ = tx.Rollback()
				t.Fatalf("seed %d/%s: %v", i, kind, err)
			}
			_ = tx.Commit()
		}
	}
	return s
}

func TestRunReflect_HappyPathUsesAllSessionsInWindow(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 5)
	f := &fakeLLM{reply: "Top 3 tasks:\n..."}

	var out bytes.Buffer
	id, err := RunReflect(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		ReflectOptions{Since: 6 * time.Hour, Limit: 10},
		&out,
	)
	if err != nil {
		t.Fatalf("RunReflect: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero id")
	}
	if !strings.Contains(f.lastReq.Messages[0].Content, "sessions") {
		t.Errorf("prompt missing expected body:\n%s", f.lastReq.Messages[0].Content)
	}
	if !strings.Contains(out.String(), "Top 3 tasks") {
		t.Errorf("stdout missing reply:\n%s", out.String())
	}

	// Row stored with session_id NULL (multi-session output).
	var sessNull bool
	_ = s.DB().QueryRow(
		`SELECT session_id IS NULL FROM llm_outputs WHERE id = ?`, id,
	).Scan(&sessNull)
	if !sessNull {
		t.Error("reflect output should have session_id = NULL")
	}
}

func TestRunReflect_EmptyWindowReturnsError(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 3)
	_, err := RunReflect(context.Background(), s,
		func() (llm.Client, error) { return &fakeLLM{}, nil },
		ReflectOptions{Since: time.Minute, Limit: 10},
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "no sessions") {
		t.Errorf("expected 'no sessions' error, got %v", err)
	}
}

func TestRunReflect_CacheHitSkipsLLM(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 4)
	f := &fakeLLM{reply: "cached reflect"}
	newClient := func() (llm.Client, error) { return f, nil }

	if _, err := RunReflect(context.Background(), s, newClient,
		ReflectOptions{Since: 10 * time.Hour, Limit: 10}, &bytes.Buffer{}); err != nil {
		t.Fatalf("first: %v", err)
	}
	var out bytes.Buffer
	if _, err := RunReflect(context.Background(), s, newClient,
		ReflectOptions{Since: 10 * time.Hour, Limit: 10}, &out); err != nil {
		t.Fatalf("second: %v", err)
	}
	if f.called != 1 {
		t.Errorf("cache miss on second run: calls=%d", f.called)
	}
	if !strings.Contains(out.String(), "cached reflect") {
		t.Errorf("stdout should replay cached body:\n%s", out.String())
	}
}

func TestRunReflect_LimitCapsDigestsSentToLLM(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 8)
	f := &fakeLLM{reply: "ok"}
	if _, err := RunReflect(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		ReflectOptions{Since: 24 * time.Hour, Limit: 3},
		&bytes.Buffer{},
	); err != nil {
		t.Fatalf("RunReflect: %v", err)
	}
	// Each digest heading in the prompt is "## Session N — <id>".
	body := f.lastReq.Messages[0].Content
	count := strings.Count(body, "## Session ")
	if count != 3 {
		t.Errorf("expected 3 digests in prompt, got %d", count)
	}
}

func TestRunReflect_PrefersExistingSummaryOverFirstPrompt(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 2)

	// Write a fake summary for one of the sessions — should appear
	// in the reflect prompt as "Prior summary:" rather than just
	// the first user_prompt.
	var sessID string
	_ = s.DB().QueryRow(`SELECT id FROM sessions LIMIT 1`).Scan(&sessID)
	tx, _ := s.DB().Begin()
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   sqlStringValid(sessID),
		Kind:        store.LLMKindSummary,
		Model:       "m",
		PromptHash:  "hash-for-summary",
		Body:        "SUMMARY_MARKER_XYZ",
		CreatedAtMs: time.Now().UnixMilli(),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed summary: %v", err)
	}
	_ = tx.Commit()

	f := &fakeLLM{reply: "ok"}
	if _, err := RunReflect(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		ReflectOptions{Since: 10 * time.Hour, Limit: 10},
		&bytes.Buffer{},
	); err != nil {
		t.Fatalf("RunReflect: %v", err)
	}
	body := f.lastReq.Messages[0].Content
	if !strings.Contains(body, "SUMMARY_MARKER_XYZ") {
		t.Errorf("expected prior summary in prompt:\n%s", body)
	}
	if !strings.Contains(body, "Prior summary:") {
		t.Error("expected 'Prior summary:' label for sessions that have one")
	}
}

// --- RunPropose ---

func TestRunPropose_HappyPathPersistsWithKindPropose(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 3)
	f := &fakeLLM{reply: "Skills:\n- foo"}
	id, err := RunPropose(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		ProposeOptions{Since: 5 * time.Hour, Limit: 10},
		&bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("RunPropose: %v", err)
	}
	var kind string
	_ = s.DB().QueryRow(`SELECT kind FROM llm_outputs WHERE id = ?`, id).Scan(&kind)
	if kind != string(store.LLMKindPropose) {
		t.Errorf("kind: got %q, want %q", kind, store.LLMKindPropose)
	}
}

func TestRunPropose_EmptyWindowReturnsError(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, err := RunPropose(context.Background(), s,
		func() (llm.Client, error) { return &fakeLLM{}, nil },
		ProposeOptions{Since: time.Hour, Limit: 10},
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "no sessions") {
		t.Errorf("expected 'no sessions' error, got %v", err)
	}
}

func TestRunPropose_LLMErrorLeavesDBClean(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 2)
	f := &fakeLLM{err: errors.New("network gone")}
	_, err := RunPropose(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		ProposeOptions{Since: 10 * time.Hour, Limit: 10},
		&bytes.Buffer{},
	)
	if err == nil {
		t.Fatal("expected error")
	}
	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM llm_outputs`).Scan(&n)
	if n != 0 {
		t.Errorf("expected 0 rows after failure, got %d", n)
	}
}

func TestRunPropose_ReflectAndProposeCoexistUnderSameInput(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 3)
	newClient := func() (llm.Client, error) { return &fakeLLM{reply: "r"}, nil }

	if _, err := RunReflect(context.Background(), s, newClient,
		ReflectOptions{Since: 10 * time.Hour, Limit: 10}, &bytes.Buffer{}); err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if _, err := RunPropose(context.Background(), s, newClient,
		ProposeOptions{Since: 10 * time.Hour, Limit: 10}, &bytes.Buffer{}); err != nil {
		t.Fatalf("propose: %v", err)
	}

	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM llm_outputs`).Scan(&n)
	if n != 2 {
		t.Errorf("expected 2 rows (one reflect, one propose), got %d", n)
	}
}

// sqlStringValid returns a valid sql.NullString around s; tiny
// convenience to keep test fixtures readable.
func sqlStringValid(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
