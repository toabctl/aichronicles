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

func TestLoadLLMOutputs_NewestFirstAcrossAllKinds(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	now := time.Now().UnixMilli()
	fixtures := []struct {
		kind LLMOutputKind
		hash string
		body string
		ts   int64
	}{
		{LLMKindSummary, "hA", "oldest", now - 3000},
		{LLMKindReflect, "hB", "middle", now - 2000},
		{LLMKindPropose, "hC", "newest", now - 1000},
	}
	withTx(t, s, func(tx *sql.Tx) {
		for _, fx := range fixtures {
			out := newOutput(fx.kind, fx.hash, fx.body)
			out.CreatedAtMs = fx.ts
			if _, _, err := SaveLLMOutput(t.Context(), tx, out); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	})

	got, err := LoadLLMOutputs(t.Context(), s.DB(), LLMOutputFilter{})
	if err != nil {
		t.Fatalf("LoadLLMOutputs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("count: got %d, want 3", len(got))
	}
	// newest → oldest
	wantOrder := []string{"newest", "middle", "oldest"}
	for i, w := range wantOrder {
		if got[i].Body != w {
			t.Errorf("row %d: got body %q, want %q", i, got[i].Body, w)
		}
	}
}

func TestLoadLLMOutputs_FilterByKind(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	withTx(t, s, func(tx *sql.Tx) {
		for _, fx := range []struct {
			kind LLMOutputKind
			hash string
		}{
			{LLMKindSummary, "s1"},
			{LLMKindReflect, "r1"},
			{LLMKindSummary, "s2"},
			{LLMKindPropose, "p1"},
		} {
			if _, _, err := SaveLLMOutput(t.Context(), tx, newOutput(fx.kind, fx.hash, "x")); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	})

	got, err := LoadLLMOutputs(t.Context(), s.DB(), LLMOutputFilter{Kind: LLMKindSummary})
	if err != nil {
		t.Fatalf("LoadLLMOutputs: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("summary rows: got %d, want 2", len(got))
	}
	for _, r := range got {
		if r.Kind != LLMKindSummary {
			t.Errorf("stray kind %q", r.Kind)
		}
	}
}

func TestLoadLLMOutputs_FilterBySession(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Seed a real session so the foreign key stands up.
	seedEvents(t, s, "outputs-filter", 1, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sessID := "claude-code/outputs-filter"
	// Derive the canonical id the way ingest does — same helper as
	// elsewhere in the store layer.
	var realSID string
	_ = s.DB().QueryRow(`SELECT id FROM sessions WHERE source_session_id = ?`, "outputs-filter").Scan(&realSID)

	withTx(t, s, func(tx *sql.Tx) {
		a := newOutput(LLMKindSummary, "with-session", "A")
		a.SessionID = sql.NullString{String: realSID, Valid: true}
		b := newOutput(LLMKindReflect, "no-session", "B") // multi-session → NULL
		if _, _, err := SaveLLMOutput(t.Context(), tx, a); err != nil {
			t.Fatalf("seed A: %v", err)
		}
		if _, _, err := SaveLLMOutput(t.Context(), tx, b); err != nil {
			t.Fatalf("seed B: %v", err)
		}
	})

	got, err := LoadLLMOutputs(t.Context(), s.DB(), LLMOutputFilter{SessionID: realSID})
	if err != nil {
		t.Fatalf("LoadLLMOutputs: %v", err)
	}
	if len(got) != 1 || got[0].Body != "A" {
		t.Errorf("session filter: got %+v", got)
	}

	// With no session filter, the NULL-session row is included too.
	all, _ := LoadLLMOutputs(t.Context(), s.DB(), LLMOutputFilter{})
	if len(all) != 2 {
		t.Errorf("all rows: got %d, want 2 (including NULL session)", len(all))
	}
	_ = sessID // silence unused complaint if seedEvents changes
}

func TestLoadLLMOutputs_LimitClamped(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	withTx(t, s, func(tx *sql.Tx) {
		for i := range 5 {
			out := newOutput(LLMKindSummary, "h"+string(rune('0'+i)), "x")
			out.CreatedAtMs = int64(i + 1)
			if _, _, err := SaveLLMOutput(t.Context(), tx, out); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	})
	got, err := LoadLLMOutputs(t.Context(), s.DB(), LLMOutputFilter{Limit: 2})
	if err != nil {
		t.Fatalf("LoadLLMOutputs: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("explicit limit: got %d, want 2", len(got))
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

func TestLoadSummariesIndexedByID_EmptyInputReturnsEmptyMap(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	got, err := LoadSummariesIndexedByID(t.Context(), s.DB(), nil)
	if err != nil {
		t.Fatalf("LoadSummariesIndexedByID(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}

	got, err = LoadSummariesIndexedByID(t.Context(), s.DB(), []string{})
	if err != nil {
		t.Fatalf("LoadSummariesIndexedByID(empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestLoadSummariesIndexedByID_OnePerSessionNewestWins(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Three sessions: A has two summaries (we expect the newest),
	// B has one, C has none — C must be absent from the result map.
	for _, id := range []string{"sess-A", "sess-B", "sess-C"} {
		if _, err := s.DB().Exec(
			`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, ?, ?)`,
			id, "claude-code", id,
		); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	mkSummary := func(sessID, hash, body string, ts int64) *LLMOutput {
		o := newOutput(LLMKindSummary, hash, body)
		o.SessionID = sql.NullString{String: sessID, Valid: true}
		o.CreatedAtMs = ts
		return o
	}

	withTx(t, s, func(tx *sql.Tx) {
		fixtures := []*LLMOutput{
			mkSummary("sess-A", "A-old", "A-OLD-BODY", 100),
			mkSummary("sess-A", "A-new", "A-NEW-BODY", 200),
			mkSummary("sess-B", "B-only", "B-BODY", 150),
			// sess-C: no summary
		}
		for _, fx := range fixtures {
			if _, _, err := SaveLLMOutput(t.Context(), tx, fx); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	})

	got, err := LoadSummariesIndexedByID(t.Context(), s.DB(),
		[]string{"sess-A", "sess-B", "sess-C"})
	if err != nil {
		t.Fatalf("LoadSummariesIndexedByID: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("entry count: got %d, want 2 (A and B; C has no summary)", len(got))
	}
	if a, ok := got["sess-A"]; !ok {
		t.Error("missing sess-A entry")
	} else if a.PromptHash != "A-new" {
		t.Errorf("sess-A: expected newest summary; got hash %q body %q", a.PromptHash, a.Body)
	}
	if b, ok := got["sess-B"]; !ok {
		t.Error("missing sess-B entry")
	} else if b.PromptHash != "B-only" {
		t.Errorf("sess-B: got hash %q", b.PromptHash)
	}
	if _, ok := got["sess-C"]; ok {
		t.Error("sess-C has no summary; should be absent from map")
	}
}

func TestLoadSummariesIndexedByID_IgnoresNonSummaryKinds(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, ?, ?)`,
		"sess-mix", "claude-code", "mix",
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Plant a reflect output AND a propose output for sess-mix —
	// neither should appear in the result. No summary → absent.
	withTx(t, s, func(tx *sql.Tx) {
		for _, fx := range []*LLMOutput{
			func() *LLMOutput {
				o := newOutput(LLMKindReflect, "r1", "reflect body")
				o.SessionID = sql.NullString{String: "sess-mix", Valid: true}
				return o
			}(),
			func() *LLMOutput {
				o := newOutput(LLMKindPropose, "p1", "propose body")
				o.SessionID = sql.NullString{String: "sess-mix", Valid: true}
				return o
			}(),
		} {
			if _, _, err := SaveLLMOutput(t.Context(), tx, fx); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	})

	got, err := LoadSummariesIndexedByID(t.Context(), s.DB(), []string{"sess-mix"})
	if err != nil {
		t.Fatalf("LoadSummariesIndexedByID: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("non-summary kinds must be ignored; got %v", got)
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
