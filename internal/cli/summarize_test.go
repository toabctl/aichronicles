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

	"github.com/toabctl/aichronicles/internal/ingest"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/store"
)

// --- fake LLM client ---

type fakeLLM struct {
	reply   string
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
	return &llm.Response{
		Text:  f.reply,
		Model: "claude-sonnet-4-6",
		Usage: llm.Usage{InputTokens: 17, OutputTokens: 23},
	}, nil
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

func TestRunSummarize_NoEventsIsError(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, err := RunSummarize(context.Background(), s,
		func() (llm.Client, error) { return &fakeLLM{}, nil },
		SummarizeOptions{SessionID: "nope"},
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "no events") {
		t.Fatalf("expected 'no events' error, got %v", err)
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
