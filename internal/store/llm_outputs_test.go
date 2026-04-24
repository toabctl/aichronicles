package store

import (
	"database/sql"
	"testing"
	"time"
)

func newOutput(kind LLMOutputKind, hash, body string) *LLMOutput {
	return &LLMOutput{
		Kind:         kind,
		Model:        "claude-sonnet-4-6",
		PromptHash:   hash,
		InputTokens:  sql.NullInt64{Int64: 100, Valid: true},
		OutputTokens: sql.NullInt64{Int64: 50, Valid: true},
		Body:         body,
		CreatedAtMs:  time.Now().UnixMilli(),
	}
}

func TestSaveLLMOutput_HappyPathInsertsRow(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	withTx(t, s, func(tx *sql.Tx) {
		id, inserted, err := SaveLLMOutput(t.Context(), tx, newOutput(LLMKindSummary, "h1", "body one"))
		if err != nil {
			t.Fatalf("SaveLLMOutput: %v", err)
		}
		if !inserted {
			t.Error("expected inserted=true")
		}
		if id == 0 {
			t.Error("expected non-zero id")
		}
	})
}

func TestSaveLLMOutput_DuplicateHashReturnsExisting(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	var firstID int64
	withTx(t, s, func(tx *sql.Tx) {
		id, _, err := SaveLLMOutput(t.Context(), tx, newOutput(LLMKindSummary, "dup", "first"))
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		firstID = id
	})
	withTx(t, s, func(tx *sql.Tx) {
		id, inserted, err := SaveLLMOutput(t.Context(), tx, newOutput(LLMKindSummary, "dup", "second"))
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		if inserted {
			t.Error("expected inserted=false on duplicate (kind, prompt_hash)")
		}
		if id != firstID {
			t.Errorf("id: got %d, want %d (same row)", id, firstID)
		}
	})

	// Body MUST NOT have been overwritten — dedup is read-only.
	var got string
	_ = s.DB().QueryRow(`SELECT body FROM llm_outputs WHERE id = ?`, firstID).Scan(&got)
	if got != "first" {
		t.Errorf("body overwritten: got %q, want %q", got, "first")
	}
}

func TestSaveLLMOutput_SameHashDifferentKindCoexists(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	withTx(t, s, func(tx *sql.Tx) {
		if _, _, err := SaveLLMOutput(t.Context(), tx, newOutput(LLMKindSummary, "shared", "as summary")); err != nil {
			t.Fatalf("summary: %v", err)
		}
		if _, _, err := SaveLLMOutput(t.Context(), tx, newOutput(LLMKindReflect, "shared", "as reflect")); err != nil {
			t.Fatalf("reflect: %v", err)
		}
	})
	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM llm_outputs WHERE prompt_hash = 'shared'`).Scan(&n)
	if n != 2 {
		t.Errorf("expected 2 rows, got %d", n)
	}
}

func TestSaveLLMOutput_ValidationErrors(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	withTx(t, s, func(tx *sql.Tx) {
		if _, _, err := SaveLLMOutput(t.Context(), tx, nil); err == nil {
			t.Error("expected error for nil output")
		}
		if _, _, err := SaveLLMOutput(t.Context(), tx, &LLMOutput{PromptHash: "x", Body: "y"}); err == nil {
			t.Error("expected error for missing kind")
		}
		if _, _, err := SaveLLMOutput(t.Context(), tx, &LLMOutput{Kind: LLMKindSummary, Body: "y"}); err == nil {
			t.Error("expected error for missing prompt_hash")
		}
		if _, _, err := SaveLLMOutput(t.Context(), tx, &LLMOutput{Kind: LLMKindSummary, PromptHash: "x"}); err == nil {
			t.Error("expected error for missing body")
		}
	})
}

func TestLoadLLMOutputByHash_ReturnsNilWhenAbsent(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	got, err := LoadLLMOutputByHash(t.Context(), s.DB(), LLMKindSummary, "nope")
	if err != nil {
		t.Fatalf("LoadLLMOutputByHash: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestLoadLLMOutputByHash_ReturnsRowWhenPresent(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	withTx(t, s, func(tx *sql.Tx) {
		if _, _, err := SaveLLMOutput(t.Context(), tx, newOutput(LLMKindSummary, "h42", "hello")); err != nil {
			t.Fatalf("seed: %v", err)
		}
	})
	got, err := LoadLLMOutputByHash(t.Context(), s.DB(), LLMKindSummary, "h42")
	if err != nil {
		t.Fatalf("LoadLLMOutputByHash: %v", err)
	}
	if got == nil {
		t.Fatal("expected a row")
	}
	if got.Body != "hello" {
		t.Errorf("body: got %q", got.Body)
	}
	if got.InputTokens.Int64 != 100 {
		t.Errorf("input_tokens: got %+v", got.InputTokens)
	}
}

func TestLoadLLMOutputsForSession_NewestFirst(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Pre-seed a session row so the FK holds.
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, ?, ?)`,
		"sess-1", "claude-code", "test",
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	for i, hash := range []string{"h-old", "h-mid", "h-new"} {
		o := newOutput(LLMKindSummary, hash, "body-"+hash)
		o.SessionID = sql.NullString{String: "sess-1", Valid: true}
		o.CreatedAtMs = int64(100 + i*10)
		withTx(t, s, func(tx *sql.Tx) {
			if _, _, err := SaveLLMOutput(t.Context(), tx, o); err != nil {
				t.Fatalf("seed %s: %v", hash, err)
			}
		})
	}

	got, err := LoadLLMOutputsForSession(t.Context(), s.DB(), "sess-1")
	if err != nil {
		t.Fatalf("LoadLLMOutputsForSession: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(got))
	}
	if got[0].PromptHash != "h-new" {
		t.Errorf("newest first: got %q, want h-new", got[0].PromptHash)
	}
	if got[2].PromptHash != "h-old" {
		t.Errorf("oldest last: got %q, want h-old", got[2].PromptHash)
	}
}

func TestLLMOutputs_SessionDeleteDetachesNotCascades(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, ?, ?)`,
		"sess-x", "claude-code", "test",
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	o := newOutput(LLMKindSummary, "keep-me", "summary body")
	o.SessionID = sql.NullString{String: "sess-x", Valid: true}
	withTx(t, s, func(tx *sql.Tx) {
		if _, _, err := SaveLLMOutput(t.Context(), tx, o); err != nil {
			t.Fatalf("seed: %v", err)
		}
	})

	// Delete the session. The llm_outputs row should survive with
	// session_id set to NULL (historical record intact).
	if _, err := s.DB().Exec(`DELETE FROM sessions WHERE id = ?`, "sess-x"); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	got, err := LoadLLMOutputByHash(t.Context(), s.DB(), LLMKindSummary, "keep-me")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got == nil {
		t.Fatal("output should survive session delete")
	}
	if got.SessionID.Valid {
		t.Errorf("session_id should be NULL after parent delete, got %+v", got.SessionID)
	}
}
