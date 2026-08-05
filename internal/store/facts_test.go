package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLoadRecentFacts_OffsetPaginates(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)

	const n = 5
	for i := range n {
		if _, err := SaveSemanticFact(ctx, s.DB(), SemanticFact{
			SourceLLMOutputID: loID,
			Subject:           fmt.Sprintf("/proj/%d", i),
			Predicate:         "primary_language",
			Object:            "Go",
			Confidence:        1.0,
			AssertedAtMs:      int64(1_700_000_000_000 + i*1000),
		}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	full, err := LoadRecentFacts(ctx, s.DB(), 100, 0)
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if len(full) != n {
		t.Fatalf("full: got %d want %d", len(full), n)
	}

	var paged []SemanticFact
	seen := map[int64]bool{}
	for off := 0; ; off += 2 {
		page, err := LoadRecentFacts(ctx, s.DB(), 2, off)
		if err != nil {
			t.Fatalf("offset %d: %v", off, err)
		}
		for _, f := range page {
			if seen[f.ID] {
				t.Fatalf("fact id %d appeared twice across pages", f.ID)
			}
			seen[f.ID] = true
		}
		paged = append(paged, page...)
		if len(page) < 2 {
			break
		}
	}
	if len(paged) != n {
		t.Fatalf("paged total: got %d want %d", len(paged), n)
	}
	for i := range full {
		if full[i].ID != paged[i].ID {
			t.Errorf("order mismatch at %d", i)
		}
	}
}

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
		EvidenceQuote:     ptrTo("go.mod requires 1.26"),
		AssertedAtMs:      1_700_000_000_000,
	}
	id, err := SaveSemanticFact(ctx, s.DB(), fact)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero row id")
	}

	got, err := LoadFactsForSubject(ctx, s.DB(), "/work/aichronicles", 0, 0)
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
		{"session without quote", SemanticFact{
			SourceLLMOutputID: loID, Subject: "s", Predicate: "p", Object: "o",
			AssertedAtMs: 1, Confidence: 1,
			EvidenceSessionID: ptrTo("00000000-0000-0000-0000-000000000001"),
			// EvidenceQuote intentionally nil
		}},
		{"session with empty quote", SemanticFact{
			SourceLLMOutputID: loID, Subject: "s", Predicate: "p", Object: "o",
			AssertedAtMs: 1, Confidence: 1,
			EvidenceSessionID: ptrTo("00000000-0000-0000-0000-000000000002"),
			EvidenceQuote:     ptrTo(""),
		}},
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

	got, err := LoadFactsForSubject(ctx, s.DB(), "/work/proj", 0, 0)
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

	got, err := LoadFactsForSubject(ctx, s.DB(), "/work/proj", 0, 0)
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
		EvidenceSessionID: ptrTo(sessID),
		EvidenceQuote:     ptrTo("ran go test ./... cleanly"),
		AssertedAtMs:      1_700_000_000_000,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Delete the session; the evidence pointer should NULL out, but
	// the fact must survive (the LLM_output still claims it).
	if _, err := s.DB().Exec(`DELETE FROM sessions WHERE id = ?`, sessID); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	got, err := LoadFactsForSubject(ctx, s.DB(), "/work/proj", 0, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("fact must survive session deletion: got %d rows", len(got))
	}
	if got[0].EvidenceSessionID != nil {
		t.Errorf("evidence_session_id must NULL on cascade: got %v", *got[0].EvidenceSessionID)
	}
}

// TestSemanticFacts_SchemaEnforcesQuoteWhenSessionSet bypasses the
// Go writer guard (SaveSemanticFact rejects this in code) and
// asserts that the SQL CHECK constraint added in migration 029
// also rejects it. Two writers must agree: if a future migration,
// direct INSERT, or new code path forgets the Go check, the table
// is still safe.
func TestSemanticFacts_SchemaEnforcesQuoteWhenSessionSet(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)

	const sessID = "00000000-0000-0000-0000-000000000def"
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, 'claude-code', 'src-y')`,
		sessID,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	cases := []struct {
		name  string
		quote any
	}{
		{"null quote", nil},
		{"empty quote", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.DB().ExecContext(ctx,
				`INSERT INTO semantic_facts(
                     source_llm_output_id, subject, predicate, object,
                     evidence_session_id, evidence_quote, asserted_at_ms
                 ) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				loID, "/work/proj", "runs_tests_via", "go test ./...",
				sessID, tc.quote, 1_700_000_000_000,
			)
			if err == nil {
				t.Fatal("expected CHECK constraint to reject session+empty-quote, got nil")
			}
			// SQLite reports CHECK violations with "CHECK constraint" in
			// the message. We assert on the shape rather than the exact
			// text so a future driver upgrade doesn't break us.
			if !strings.Contains(err.Error(), "CHECK") && !strings.Contains(err.Error(), "constraint") {
				t.Errorf("error doesn't look like a constraint violation: %v", err)
			}
		})
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

	got, err := LoadFactsForSubject(ctx, s.DB(), "/work/proj", 0, 0)
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

	got, err := LoadFactsForSubject(ctx, s.DB(), "/work/proj", 0, 0)
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
	got, err := LoadRecentFacts(ctx, s.DB(), 0, 0)
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

// TestLoadDistinctFactSubjects_ReturnsSortedDistinct covers the
// no-needle variant added for the web's facts-index page. Subjects
// must be returned in alphabetical order, dedup'd across multiple
// facts for the same subject.
func TestLoadDistinctFactSubjects_ReturnsSortedDistinct(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)

	// Three subjects, with two facts on one of them — dedup must
	// collapse to three rows in alphabetical order.
	rows := []SemanticFact{
		{SourceLLMOutputID: loID, Subject: "/work/zebra", Predicate: "primary_language", Object: "Go", Confidence: 1, AssertedAtMs: 1},
		{SourceLLMOutputID: loID, Subject: "/work/alpha", Predicate: "primary_language", Object: "Go", Confidence: 1, AssertedAtMs: 1},
		{SourceLLMOutputID: loID, Subject: "/work/middle", Predicate: "primary_language", Object: "Go", Confidence: 1, AssertedAtMs: 1},
		{SourceLLMOutputID: loID, Subject: "/work/alpha", Predicate: "framework", Object: "stdlib", Confidence: 1, AssertedAtMs: 2},
	}
	for _, r := range rows {
		if _, err := SaveSemanticFact(ctx, s.DB(), r); err != nil {
			t.Fatalf("save %s: %v", r.Subject, err)
		}
	}

	got, err := LoadDistinctFactSubjects(ctx, s.DB(), 0)
	if err != nil {
		t.Fatalf("LoadDistinctFactSubjects: %v", err)
	}
	want := []string{"/work/alpha", "/work/middle", "/work/zebra"}
	if len(got) != len(want) {
		t.Fatalf("len=%d got=%v want=%v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("subject[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestLoadDistinctFactSubjects_EmptyTable verifies the empty-DB
// case returns (nil, nil) rather than an error.
func TestLoadDistinctFactSubjects_EmptyTable(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	got, err := LoadDistinctFactSubjects(context.Background(), s.DB(), 0)
	if err != nil {
		t.Fatalf("LoadDistinctFactSubjects: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestLoadFactsForSubject_RejectsEmptySubject(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	if _, err := LoadFactsForSubject(context.Background(), s.DB(), "", 0, 0); err == nil {
		t.Errorf("expected error for empty subject")
	}
}

// TestSaveSemanticFact_OlderAssertionDoesNotWin pins the
// last-write-wins semantics the doc comment promises.
//
// The upsert unconditionally overwrote asserted_at_ms, confidence,
// source_llm_output_id and the evidence pointer, so a write carrying
// an OLDER timestamp moved the fact backwards in time. Because
// LoadFactsForSubject resolves competing objects by asserted_at_ms
// DESC, that silently changed which value the retrieval layer treats
// as current.
//
// The in-tree induce path always passes time.Now(), so this is not
// reachable from today's CLI — but POST /v1/facts takes a
// client-supplied asserted_at_ms with no validation, and any replay
// or backfill writer would hit it.
func TestSaveSemanticFact_OlderAssertionDoesNotWin(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sessionID := ingestForScrub(t, s, "no-secret", nil)
	outputID := insertLLMOutputForTest(t, s, sessionID)

	newQuote := "the newer evidence"
	oldQuote := "the older evidence"
	base := SemanticFact{
		SourceLLMOutputID: outputID,
		Subject:           "/tmp/proj",
		Predicate:         "runs_tests_via",
		Object:            "go test ./...",
	}

	newer := base
	newer.Confidence = 0.9
	newer.EvidenceQuote = &newQuote
	newer.AssertedAtMs = 2_000
	if _, err := SaveSemanticFact(ctx, s.DB(), newer); err != nil {
		t.Fatalf("save newer: %v", err)
	}

	older := base
	older.Confidence = 0.1
	older.EvidenceQuote = &oldQuote
	older.AssertedAtMs = 1_000
	if _, err := SaveSemanticFact(ctx, s.DB(), older); err != nil {
		t.Fatalf("save older must not error: %v", err)
	}

	var gotTs int64
	var gotConf float64
	var gotQuote string
	if err := s.DB().QueryRow(
		`SELECT asserted_at_ms, confidence, COALESCE(evidence_quote, '')
		   FROM semantic_facts
		  WHERE subject = ? AND predicate = ? AND object = ?`,
		base.Subject, base.Predicate, base.Object,
	).Scan(&gotTs, &gotConf, &gotQuote); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if gotTs != 2_000 {
		t.Errorf("asserted_at_ms moved backwards: got %d, want 2000", gotTs)
	}
	if gotConf != 0.9 {
		t.Errorf("confidence was rebound to the older assertion: got %v, want 0.9", gotConf)
	}
	if gotQuote != newQuote {
		t.Errorf("evidence pointer was rebound: got %q, want %q", gotQuote, newQuote)
	}
}

// TestSaveSemanticFact_EqualOrNewerStillWins guards the other
// direction: the guard must not block a legitimate refresh, including
// a same-millisecond re-assertion.
func TestSaveSemanticFact_EqualOrNewerStillWins(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sessionID := ingestForScrub(t, s, "no-secret", nil)
	outputID := insertLLMOutputForTest(t, s, sessionID)

	base := SemanticFact{
		SourceLLMOutputID: outputID,
		Subject:           "/tmp/proj",
		Predicate:         "runs_tests_via",
		Object:            "go test ./...",
		Confidence:        0.5,
		AssertedAtMs:      1_000,
	}
	if _, err := SaveSemanticFact(ctx, s.DB(), base); err != nil {
		t.Fatalf("save: %v", err)
	}

	for _, tc := range []struct {
		name string
		ts   int64
		conf float64
	}{
		{"same timestamp", 1_000, 0.6},
		{"newer timestamp", 3_000, 0.8},
	} {
		f := base
		f.AssertedAtMs = tc.ts
		f.Confidence = tc.conf
		if _, err := SaveSemanticFact(ctx, s.DB(), f); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var conf float64
		if err := s.DB().QueryRow(
			`SELECT confidence FROM semantic_facts
			  WHERE subject = ? AND predicate = ? AND object = ?`,
			base.Subject, base.Predicate, base.Object).Scan(&conf); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if conf != tc.conf {
			t.Errorf("%s should have been applied: confidence %v, want %v",
				tc.name, conf, tc.conf)
		}
	}
}
