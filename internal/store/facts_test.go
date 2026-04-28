package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// mkFactsRow seeds a minimal llm_outputs row of kind=facts so a
// SemanticFact's source_llm_output_id FK is satisfied.
func mkFactsRow(t *testing.T, s *Store, createdAtMs int64) int64 {
	t.Helper()
	return seedLLMOutput(t, s, "facts", "test-model", 0, 0,
		time.UnixMilli(createdAtMs).UTC())
}

func TestSaveSemanticFact_Roundtrip(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)

	fact := SemanticFact{
		SourceLLMOutputID: loID,
		Subject:           "/work/aichronicles",
		Predicate:         "uses_language_version",
		Object:            "Go 1.26",
		Confidence:        0.95,
		EvidenceQuote:     sql.NullString{String: "go.mod requires 1.26", Valid: true},
		AssertedAtMs:      1_700_000_000_000,
	}
	id, err := SaveSemanticFact(ctx, s.DB(), fact)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero row id")
	}

	got, err := LoadFactsForSubject(ctx, s.DB(), "/work/aichronicles", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows: got %d want 1", len(got))
	}
	r := got[0]
	if r.Subject != fact.Subject || r.Predicate != fact.Predicate || r.Object != fact.Object {
		t.Errorf("fact triple roundtrip wrong: got %+v", r)
	}
	if r.Confidence != 0.95 {
		t.Errorf("confidence: got %v want 0.95", r.Confidence)
	}
}

func TestSaveSemanticFact_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)

	cases := []struct {
		name string
		f    SemanticFact
	}{
		{"missing source", SemanticFact{Subject: "x", Predicate: "p", Object: "o", AssertedAtMs: 1, Confidence: 1}},
		{"missing subject", SemanticFact{SourceLLMOutputID: loID, Predicate: "p", Object: "o", AssertedAtMs: 1, Confidence: 1}},
		{"missing predicate", SemanticFact{SourceLLMOutputID: loID, Subject: "s", Object: "o", AssertedAtMs: 1, Confidence: 1}},
		{"missing object", SemanticFact{SourceLLMOutputID: loID, Subject: "s", Predicate: "p", AssertedAtMs: 1, Confidence: 1}},
		{"missing asserted", SemanticFact{SourceLLMOutputID: loID, Subject: "s", Predicate: "p", Object: "o", Confidence: 1}},
		{"confidence too low", SemanticFact{SourceLLMOutputID: loID, Subject: "s", Predicate: "p", Object: "o", AssertedAtMs: 1, Confidence: -0.01}},
		{"confidence too high", SemanticFact{SourceLLMOutputID: loID, Subject: "s", Predicate: "p", Object: "o", AssertedAtMs: 1, Confidence: 1.01}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := SaveSemanticFact(ctx, s.DB(), c.f); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

func TestSaveSemanticFact_RepeatTripleUpdatesInPlace(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)

	first := SemanticFact{
		SourceLLMOutputID: loID,
		Subject:           "/work/proj",
		Predicate:         "primary_language",
		Object:            "Go",
		Confidence:        0.8,
		AssertedAtMs:      1_700_000_000_000,
	}
	id1, err := SaveSemanticFact(ctx, s.DB(), first)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	// Re-assert the same triple later with higher confidence.
	second := first
	second.Confidence = 0.99
	second.AssertedAtMs = 1_700_000_500_000
	id2, err := SaveSemanticFact(ctx, s.DB(), second)
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if id1 != id2 {
		t.Errorf("re-assertion should reuse the row id: got %d, want %d", id2, id1)
	}

	got, err := LoadFactsForSubject(ctx, s.DB(), "/work/proj", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows: got %d want 1 (PK invariant)", len(got))
	}
	if got[0].Confidence != 0.99 || got[0].AssertedAtMs != 1_700_000_500_000 {
		t.Errorf("update did not refresh fields: %+v", got[0])
	}
}

func TestSaveSemanticFact_ConflictingObjectsCoexist(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)

	// Same subject + predicate, different object — both must
	// persist as separate rows. The truth is "I've seen both
	// values"; the caller picks by asserted_at_ms.
	for _, version := range []string{"Go 1.25", "Go 1.26"} {
		if _, err := SaveSemanticFact(ctx, s.DB(), SemanticFact{
			SourceLLMOutputID: loID,
			Subject:           "/work/proj",
			Predicate:         "uses_language_version",
			Object:            version,
			Confidence:        0.9,
			AssertedAtMs:      1_700_000_000_000,
		}); err != nil {
			t.Fatalf("save %s: %v", version, err)
		}
	}

	got, err := LoadFactsForSubject(ctx, s.DB(), "/work/proj", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("conflicting objects must coexist: got %d rows want 2", len(got))
	}
}

func TestSaveSemanticFact_EvidenceCascadesOnSessionDelete(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)

	const sessID = "00000000-0000-0000-0000-000000000abc"
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, 'claude-code', 'src-x')`,
		sessID,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := SaveSemanticFact(ctx, s.DB(), SemanticFact{
		SourceLLMOutputID: loID,
		Subject:           "/work/proj",
		Predicate:         "runs_tests_via",
		Object:            "go test ./...",
		Confidence:        1.0,
		EvidenceSessionID: sql.NullString{String: sessID, Valid: true},
		EvidenceQuote:     sql.NullString{String: "ran go test ./... cleanly", Valid: true},
		AssertedAtMs:      1_700_000_000_000,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Delete the session; the evidence pointer should NULL out, but
	// the fact must survive (the LLM_output still claims it).
	if _, err := s.DB().Exec(`DELETE FROM sessions WHERE id = ?`, sessID); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	got, err := LoadFactsForSubject(ctx, s.DB(), "/work/proj", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("fact must survive session deletion: got %d rows", len(got))
	}
	if got[0].EvidenceSessionID.Valid {
		t.Errorf("evidence_session_id must NULL on cascade: got %v", got[0].EvidenceSessionID)
	}
}

func TestSaveSemanticFact_LLMOutputDeleteCascadesFact(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)
	if _, err := SaveSemanticFact(ctx, s.DB(), SemanticFact{
		SourceLLMOutputID: loID,
		Subject:           "/work/proj",
		Predicate:         "primary_language",
		Object:            "Go",
		Confidence:        1.0,
		AssertedAtMs:      1_700_000_000_000,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Delete the LLM output — the fact should cascade-drop because
	// the LLM run that asserted it is gone.
	if _, err := s.DB().Exec(`DELETE FROM llm_outputs WHERE id = ?`, loID); err != nil {
		t.Fatalf("delete llm_output: %v", err)
	}

	got, err := LoadFactsForSubject(ctx, s.DB(), "/work/proj", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("fact should cascade-drop with its LLM_output: got %d rows", len(got))
	}
}

func TestLoadFactsForSubject_OrdersByPredicateThenAsserted(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)

	// Three predicates on the same subject, asserted at different
	// times. Expected order: predicate ASC, asserted_at DESC.
	specs := []struct {
		predicate string
		object    string
		ts        int64
	}{
		{"uses_dependency", "modernc.org/sqlite", 1_700_000_000_000},
		{"primary_language", "Go", 1_700_000_300_000},
		{"primary_language", "Python", 1_700_000_200_000}, // older — should still come second within predicate
		{"runs_tests_via", "go test ./...", 1_700_000_400_000},
	}
	for _, sp := range specs {
		if _, err := SaveSemanticFact(ctx, s.DB(), SemanticFact{
			SourceLLMOutputID: loID,
			Subject:           "/work/proj",
			Predicate:         sp.predicate,
			Object:            sp.object,
			Confidence:        1.0,
			AssertedAtMs:      sp.ts,
		}); err != nil {
			t.Fatalf("save %s=%s: %v", sp.predicate, sp.object, err)
		}
	}

	got, err := LoadFactsForSubject(ctx, s.DB(), "/work/proj", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("rows: got %d want 4", len(got))
	}
	want := []struct {
		predicate string
		object    string
	}{
		{"primary_language", "Go"},     // newer first within predicate
		{"primary_language", "Python"}, // older second
		{"runs_tests_via", "go test ./..."},
		{"uses_dependency", "modernc.org/sqlite"},
	}
	for i, w := range want {
		if got[i].Predicate != w.predicate || got[i].Object != w.object {
			t.Errorf("row %d: got (%s, %s) want (%s, %s)",
				i, got[i].Predicate, got[i].Object, w.predicate, w.object)
		}
	}
}

func TestLoadRecentFacts_OrdersByAssertedDesc(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)

	for i, sub := range []string{"/a", "/b", "/c"} {
		if _, err := SaveSemanticFact(ctx, s.DB(), SemanticFact{
			SourceLLMOutputID: loID,
			Subject:           sub,
			Predicate:         "primary_language",
			Object:            "Go",
			Confidence:        1.0,
			AssertedAtMs:      int64(1_700_000_000_000 + i*1000),
		}); err != nil {
			t.Fatalf("save %s: %v", sub, err)
		}
	}
	got, err := LoadRecentFacts(ctx, s.DB(), 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("rows: got %d want 3", len(got))
	}
	if got[0].Subject != "/c" || got[1].Subject != "/b" || got[2].Subject != "/a" {
		t.Errorf("order wrong: got %s,%s,%s", got[0].Subject, got[1].Subject, got[2].Subject)
	}
}

func TestFactSubjectsLike_CaseInsensitiveSubstring(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)

	for _, sub := range []string{
		"/work/aichronicles",
		"/work/Aichronicles-fork",
		"/work/systemd",
		"/home/u/scratch",
	} {
		if _, err := SaveSemanticFact(ctx, s.DB(), SemanticFact{
			SourceLLMOutputID: loID,
			Subject:           sub,
			Predicate:         "primary_language",
			Object:            "Go",
			Confidence:        1.0,
			AssertedAtMs:      1_700_000_000_000,
		}); err != nil {
			t.Fatalf("save %s: %v", sub, err)
		}
	}

	got, err := FactSubjectsLike(ctx, s.DB(), "aichronicles", 0)
	if err != nil {
		t.Fatalf("subjects: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 case-insensitive matches, got %d: %v", len(got), got)
	}
}

func TestFactSubjectsLike_RejectsEmptyNeedle(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	if _, err := FactSubjectsLike(context.Background(), s.DB(), "  ", 0); err == nil {
		t.Errorf("expected error for empty needle")
	}
}

func TestLoadFactsForSubject_RejectsEmptySubject(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	if _, err := LoadFactsForSubject(context.Background(), s.DB(), "", 0); err == nil {
		t.Errorf("expected error for empty subject")
	}
}
