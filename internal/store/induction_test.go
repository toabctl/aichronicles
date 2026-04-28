package store

import (
	"errors"
	"testing"
)

func seedSessionForInduction(t *testing.T, s *Store, id, cwd string, startedAt, endedAt int64, eventCount int) {
	t.Helper()
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms, cwd, event_count)
		 VALUES (?, 'claude-code', ?, ?, ?, ?, ?)`,
		id, "src-"+id, startedAt, endedAt, cwd, eventCount,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func seedInductionRow(t *testing.T, s *Store, sessionID string, ts int64) {
	t.Helper()
	if _, err := s.DB().Exec(
		`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms)
		 VALUES (?, 'induction', '{}', ?, 'fake-model', ?)`,
		sessionID, "h-"+sessionID, ts,
	); err != nil {
		t.Fatalf("seed induction: %v", err)
	}
}

func TestLoadInductionCandidates_FiltersIdleSubstantialUnprocessed(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	const baseTs = int64(1_700_000_000_000)
	const idleMs = int64(30 * 60 * 1000) // 30 min
	const nowMs = baseTs + 2*60*60*1000  // 2h after baseTs

	// Candidate A: ended 1h ago, 20 events, no induction yet — INCLUDE
	const candA = "00000000-0000-0000-0000-00000000000a"
	seedSessionForInduction(t, s, candA, "/repo", baseTs, nowMs-60*60*1000, 20)

	// Candidate B: ended 35min ago, 8 events, no induction yet — INCLUDE
	const candB = "00000000-0000-0000-0000-00000000000b"
	seedSessionForInduction(t, s, candB, "/repo", baseTs, nowMs-35*60*1000, 8)

	// Excluded: still active (ended_at within idle window — 10 min ago)
	const stillActive = "00000000-0000-0000-0000-0000000000a1"
	seedSessionForInduction(t, s, stillActive, "/repo", baseTs, nowMs-10*60*1000, 30)

	// Excluded: too small (only 2 events)
	const tooSmall = "00000000-0000-0000-0000-0000000000a2"
	seedSessionForInduction(t, s, tooSmall, "/repo", baseTs, nowMs-2*60*60*1000, 2)

	// Excluded: already has an induction row
	const alreadyDone = "00000000-0000-0000-0000-0000000000a3"
	seedSessionForInduction(t, s, alreadyDone, "/repo", baseTs, nowMs-90*60*1000, 25)
	seedInductionRow(t, s, alreadyDone, nowMs-30*60*1000)

	got, err := LoadInductionCandidates(t.Context(), s.DB(), nowMs, idleMs, 5, 10)
	if err != nil {
		t.Fatalf("LoadInductionCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates, got %d: %+v", len(got), got)
	}
	// Newest-ended first: candB ended -35min, candA ended -1h.
	if got[0].ID != candB {
		t.Errorf("got[0].ID = %q, want %q (newest-ended first)", got[0].ID, candB)
	}
	if got[1].ID != candA {
		t.Errorf("got[1].ID = %q, want %q", got[1].ID, candA)
	}
	if got[0].EventCount != 8 {
		t.Errorf("event_count carried wrong: %d", got[0].EventCount)
	}
}

func TestLoadInductionCandidates_AppliesDefaults(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const nowMs = int64(1_700_000_000_000)

	const sess = "00000000-0000-0000-0000-00000000d000"
	// Ended 1h ago, default idle threshold = 30min, default minEvents = 5.
	seedSessionForInduction(t, s, sess, "/repo", nowMs-2*60*60*1000, nowMs-60*60*1000, 6)

	got, err := LoadInductionCandidates(t.Context(), s.DB(), nowMs, 0, 0, 0)
	if err != nil {
		t.Fatalf("LoadInductionCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("with default params, expected 1 candidate, got %d", len(got))
	}
}

func TestHasInductionRun_TrueOnlyWhenRowExists(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const sess = "00000000-0000-0000-0000-00000000c000"
	seedSessionForInduction(t, s, sess, "/r", 0, 1000, 1)

	got, err := HasInductionRun(t.Context(), s.DB(), sess)
	if err != nil {
		t.Fatalf("HasInductionRun: %v", err)
	}
	if got {
		t.Errorf("expected false before any induction row, got true")
	}

	seedInductionRow(t, s, sess, 1500)
	got, err = HasInductionRun(t.Context(), s.DB(), sess)
	if err != nil {
		t.Fatalf("HasInductionRun: %v", err)
	}
	if !got {
		t.Errorf("expected true after induction row inserted, got false")
	}
}

func TestLoadInductionRow_LatestWins(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const sess = "00000000-0000-0000-0000-00000000d100"
	seedSessionForInduction(t, s, sess, "/r", 0, 1000, 1)

	// Two induction rows; newer must win.
	if _, err := s.DB().Exec(
		`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms)
		 VALUES (?, 'induction', '{"rationale":"old"}', 'h1', 'm', 1000)`,
		sess,
	); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms)
		 VALUES (?, 'induction', '{"rationale":"new"}', 'h2', 'm', 2000)`,
		sess,
	); err != nil {
		t.Fatalf("seed new: %v", err)
	}

	row, err := LoadInductionRow(t.Context(), s.DB(), sess)
	if err != nil {
		t.Fatalf("LoadInductionRow: %v", err)
	}
	if row == nil {
		t.Fatal("expected a row")
	}
	if row.CreatedAtMs != 2000 {
		t.Errorf("expected newest row (created_at_ms=2000), got %d", row.CreatedAtMs)
	}

	// Missing session → nil, no error.
	row, err = LoadInductionRow(t.Context(), s.DB(), "00000000-0000-0000-0000-deadbeef0000")
	if err != nil && !errors.Is(err, nil) {
		t.Fatalf("err: %v", err)
	}
	if row != nil {
		t.Errorf("expected nil for missing session, got %+v", row)
	}
}
