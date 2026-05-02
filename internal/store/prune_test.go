package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/pkg/events"
)

// seedSessionAt ingests one user_prompt envelope tagged with the
// given session id and timestamp. Returns the derived session id.
// Used by prune tests to populate sessions whose ended_at_ms can
// be controlled (the trigger sets it to the event's
// ts_source_ms).
func seedSessionAt(t *testing.T, s *Store, sessionKey string, ts time.Time) string {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sessionKey,
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        ts,
		ContentText:     "anchor — distinctive content " + sessionKey,
		Cwd:             "/work/" + sessionKey,
		Payload:         map[string]any{},
		Redaction:       &events.Redaction{Applied: true},
	}
	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, []byte(`{"v":1}`), env.TsSource.UnixMilli()); err != nil {
			t.Fatalf("seed: %v", err)
		}
	})
	return events.DeriveSessionID("claude-code", sessionKey)
}

// countAll returns the row counts across the four canonical
// tables. The test asserts on these directly so cascade
// behaviour is visible at a glance.
func countAll(t *testing.T, s *Store) (raw, ev, ext, sess int) {
	t.Helper()
	for table, dst := range map[string]*int{
		"raw_envelopes": &raw, "events": &ev, "extractions": &ext, "sessions": &sess,
	} {
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(dst); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
	}
	return
}

func TestPrune_DryRunCountsButDoesNotMutate(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	new := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	seedSessionAt(t, s, "old1", old)
	seedSessionAt(t, s, "new1", new)

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	rawBefore, evBefore, extBefore, sessBefore := countAll(t, s)

	r, err := Prune(t.Context(), s.DB(), PruneOptions{
		CutoffMs: cutoff,
		DryRun:   true,
	})
	if err != nil {
		t.Fatalf("Prune dry-run: %v", err)
	}
	if r.Sessions != 1 {
		t.Errorf("dry-run: would prune %d sessions, want 1", r.Sessions)
	}
	if r.RawEnvelopes != 1 || r.Events != 1 {
		t.Errorf("dry-run: raw=%d events=%d, want 1/1", r.RawEnvelopes, r.Events)
	}

	rawAfter, evAfter, extAfter, sessAfter := countAll(t, s)
	if rawAfter != rawBefore || evAfter != evBefore || extAfter != extBefore || sessAfter != sessBefore {
		t.Errorf("dry-run mutated rows (raw %d→%d, events %d→%d, sessions %d→%d, extractions %d→%d)",
			rawBefore, rawAfter, evBefore, evAfter, sessBefore, sessAfter, extBefore, extAfter)
	}
}

func TestPrune_LiveDeletesAndCascades(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	new := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	seedSessionAt(t, s, "old1", old)
	seedSessionAt(t, s, "old2", old.Add(time.Hour))
	seedSessionAt(t, s, "new1", new)

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	r, err := Prune(t.Context(), s.DB(), PruneOptions{CutoffMs: cutoff})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if r.Sessions != 2 || r.RawEnvelopes != 2 || r.Events != 2 {
		t.Errorf("counts: %+v, want sessions=2 raw=2 events=2", r)
	}

	// Final row counts: only the new session + its event survive.
	raw, ev, _, sess := countAll(t, s)
	if raw != 1 || ev != 1 || sess != 1 {
		t.Errorf("after prune: raw=%d events=%d sessions=%d, want 1/1/1", raw, ev, sess)
	}
}

// TestPrune_FTSStaysConsistent pins the load-bearing invariant
// behind the cascade: the events_fts index drops the old row
// alongside the events delete, so a search for the old session's
// content returns nothing.
func TestPrune_FTSStaysConsistent(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	new := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	seedSessionAt(t, s, "doomed", old)
	seedSessionAt(t, s, "kept", new)

	// Confirm the seeded content is searchable BEFORE prune.
	beforeOld := countFTS(t, s, "anchor doomed")
	beforeKept := countFTS(t, s, "anchor kept")
	if beforeOld != 1 || beforeKept != 1 {
		t.Fatalf("pre-prune FTS: doomed=%d kept=%d, want 1/1", beforeOld, beforeKept)
	}

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := Prune(t.Context(), s.DB(), PruneOptions{CutoffMs: cutoff}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	afterOld := countFTS(t, s, "anchor doomed")
	afterKept := countFTS(t, s, "anchor kept")
	if afterOld != 0 {
		t.Errorf("after prune: doomed should be gone from FTS, got %d hits", afterOld)
	}
	if afterKept != 1 {
		t.Errorf("after prune: kept should still be searchable, got %d hits", afterKept)
	}
}

// countFTS runs an FTS5 MATCH and returns the row count. Bare
// SQL so the test can't depend on higher-level search code that
// might mask a stale-index bug.
func countFTS(t *testing.T, s *Store, query string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM events_fts WHERE events_fts MATCH ?`, query,
	).Scan(&n); err != nil {
		t.Fatalf("FTS count: %v", err)
	}
	return n
}

// TestPrune_ProtectsActiveSessions: a session whose ended_at is
// NULL must survive a prune even if started_at is ancient.
func TestPrune_ProtectsActiveSessions(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	id := seedSessionAt(t, s, "ancient-active", old)

	// Reset ended_at_ms to NULL so the row mimics an active session.
	if _, err := s.DB().Exec(`UPDATE sessions SET ended_at_ms = NULL WHERE id = ?`, id); err != nil {
		t.Fatalf("force active: %v", err)
	}

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	r, err := Prune(t.Context(), s.DB(), PruneOptions{CutoffMs: cutoff})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if r.Sessions != 0 {
		t.Errorf("active session should be protected; pruned %d", r.Sessions)
	}

	_, _, _, sessAfter := countAll(t, s)
	if sessAfter != 1 {
		t.Errorf("active session row missing after prune (got %d)", sessAfter)
	}
}

// TestPrune_LLMOutputsSurviveByDefault: deleting a session sets
// llm_outputs.session_id to NULL via ON DELETE SET NULL; the
// row stays. --include-llm-outputs would remove it explicitly,
// covered by the next test.
func TestPrune_LLMOutputsSurviveByDefault(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	sid := seedSessionAt(t, s, "doomed", old)

	tx, _ := s.DB().Begin()
	if _, _, err := SaveLLMOutput(t.Context(), tx, &LLMOutput{
		SessionID:   sql.NullString{String: sid, Valid: true},
		Kind:        LLMKindSummary,
		Model:       "x",
		PromptHash:  "h",
		Body:        `{"topic":"t"}`,
		CreatedAtMs: old.UnixMilli(),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	_ = tx.Commit()

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := Prune(t.Context(), s.DB(), PruneOptions{CutoffMs: cutoff}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM llm_outputs`).Scan(&n)
	if n != 1 {
		t.Errorf("default prune should preserve llm_outputs (got %d)", n)
	}
	var sessNull int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM llm_outputs WHERE session_id IS NULL`).Scan(&sessNull)
	if sessNull != 1 {
		t.Errorf("orphaned llm_outputs.session_id should be NULL, got %d NULL rows", sessNull)
	}
}

func TestPrune_IncludeLLMOutputsDeletesOldOnes(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	new := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	sidOld := seedSessionAt(t, s, "old1", old)

	// Two outputs: one old (gets pruned), one new (survives).
	tx, _ := s.DB().Begin()
	for _, ts := range []time.Time{old, new} {
		if _, _, err := SaveLLMOutput(t.Context(), tx, &LLMOutput{
			SessionID:   sql.NullString{String: sidOld, Valid: true},
			Kind:        LLMKindSummary,
			Model:       "x",
			PromptHash:  ts.Format("20060102"),
			Body:        `{}`,
			CreatedAtMs: ts.UnixMilli(),
		}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	_ = tx.Commit()

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	r, err := Prune(t.Context(), s.DB(), PruneOptions{
		CutoffMs:          cutoff,
		IncludeLLMOutputs: true,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if r.LLMOutputs != 1 {
		t.Errorf("LLMOutputs report: got %d, want 1", r.LLMOutputs)
	}
	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM llm_outputs`).Scan(&n)
	if n != 1 {
		t.Errorf("after prune --include-llm-outputs: got %d rows, want 1", n)
	}
}

func TestVacuum_ReclaimsDeletedSpace(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	// Seed a meaningful number of envelopes so the page count
	// has somewhere to drop. ts spans matter so ended_at_ms
	// triggers fire.
	for i := range 50 {
		seedSessionAt(t, s, "session-"+string(rune('a'+i%26))+string(rune('a'+i/26)),
			time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Hour))
	}
	beforePrune, _ := QueryPageInfo(t.Context(), s.DB())

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := Prune(t.Context(), s.DB(), PruneOptions{CutoffMs: cutoff}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if err := Vacuum(t.Context(), s.DB()); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}

	afterVacuum, _ := QueryPageInfo(t.Context(), s.DB())
	if afterVacuum.PageCount >= beforePrune.PageCount {
		t.Errorf("vacuum should reduce page count: before=%d after=%d",
			beforePrune.PageCount, afterVacuum.PageCount)
	}
}

func TestQueryPageInfo_ReturnsPositiveValues(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	info, err := QueryPageInfo(t.Context(), s.DB())
	if err != nil {
		t.Fatalf("QueryPageInfo: %v", err)
	}
	if info.PageCount <= 0 || info.PageSize <= 0 {
		t.Errorf("expected positive values, got %+v", info)
	}
	if info.Bytes() != info.PageCount*info.PageSize {
		t.Errorf("Bytes() math wrong: %d vs %d×%d", info.Bytes(), info.PageCount, info.PageSize)
	}
}
