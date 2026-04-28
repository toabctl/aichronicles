package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/ingest/extract"
)

// mkProposeRow inserts a minimal llm_outputs row of kind=propose for
// these tests and returns the new id. Wraps the shared
// seedLLMOutput helper from token_usage_test.go with the
// propose-specific defaults this file's tests rely on.
func mkProposeRow(t *testing.T, s *Store, createdAtMs int64) int64 {
	t.Helper()
	return seedLLMOutput(t, s, string(LLMKindPropose), "test-model", 0, 0,
		time.UnixMilli(createdAtMs).UTC())
}

func TestRecordProposedSkill_Idempotent(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)

	// First call inserts; second call is a no-op via INSERT OR IGNORE
	// on the natural-key UNIQUE.
	for i := range 2 {
		if err := RecordProposedSkill(ctx, s.DB(), loID, "fix-build", 1_700_000_000_000); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM proposed_skills WHERE llm_output_id = ?`, loID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows: got %d want 1 (idempotency invariant)", n)
	}
}

func TestRecordProposedSkill_RejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
	}{
		{"empty llm_output_id", func() error { return RecordProposedSkill(ctx, s.DB(), 0, "x", 1) }},
		{"empty skill_name", func() error { return RecordProposedSkill(ctx, s.DB(), 1, "", 1) }},
		{"empty proposed_at_ms", func() error { return RecordProposedSkill(ctx, s.DB(), 1, "x", 0) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if err := c.call(); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

func TestMarkProposedSkillApplied_UpdatesRow(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordProposedSkill(ctx, s.DB(), loID, "deploy-staging", 1_700_000_000_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := MarkProposedSkillApplied(ctx, s.DB(), loID, "deploy-staging",
		"/home/u/.claude/skills/deploy-staging/SKILL.md", 1_700_000_500_000); err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	rows, err := LoadProposedSkillsByName(ctx, s.DB(), "deploy-staging", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
	r := rows[0]
	if !r.AppliedAtMs.Valid || r.AppliedAtMs.Int64 != 1_700_000_500_000 {
		t.Errorf("applied_at_ms: got %v", r.AppliedAtMs)
	}
	if !r.AppliedPath.Valid || r.AppliedPath.String != "/home/u/.claude/skills/deploy-staging/SKILL.md" {
		t.Errorf("applied_path: got %v", r.AppliedPath)
	}
}

func TestMarkProposedSkillApplied_NotFound(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	err := MarkProposedSkillApplied(context.Background(), s.DB(),
		9999, "missing", "/dev/null", 1_700_000_000_000)
	if !errors.Is(err, ErrProposedSkillNotFound) {
		t.Errorf("expected ErrProposedSkillNotFound, got %v", err)
	}
}

func TestLoadProposedSkillsByName_MultipleProposalsSameName(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	// Two propose runs over time both name the same skill — newer
	// wins for "current" but the historical row stays queryable.
	older := mkProposeRow(t, s, 1_700_000_000_000)
	newer := mkProposeRow(t, s, 1_700_001_000_000)
	if err := RecordProposedSkill(ctx, s.DB(), older, "fix-ci", 1_700_000_000_000); err != nil {
		t.Fatalf("older: %v", err)
	}
	if err := RecordProposedSkill(ctx, s.DB(), newer, "fix-ci", 1_700_001_000_000); err != nil {
		t.Fatalf("newer: %v", err)
	}

	rows, err := LoadProposedSkillsByName(ctx, s.DB(), "fix-ci", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: got %d want 2", len(rows))
	}
	// Newest first per ORDER BY proposed_at_ms DESC.
	if rows[0].LLMOutputID != newer {
		t.Errorf("ordering: rows[0].llm_output_id = %d, want %d (newer)", rows[0].LLMOutputID, newer)
	}
}

// effectivenessFixture seeds a session + a propose run + an applied
// skill + skill_load extractions with optional tool_failure pairs in
// the staleness window, so LoadProposalEffectiveness can be tested
// against a controlled distribution.
type effectivenessFixture struct {
	t        *testing.T
	s        *Store
	loID     int64
	skill    string
	session  string
	srcAgent string
	srcSess  string
	tsCursor int64
	seq      int64
	rows     int
}

func newEffectivenessFixture(t *testing.T, skill string, appliedAtMs int64) *effectivenessFixture {
	t.Helper()
	f := &effectivenessFixture{
		t:        t,
		s:        openTemp(t),
		skill:    skill,
		session:  "00000000-0000-0000-0000-000000000200",
		srcAgent: "claude-code",
		srcSess:  "src-eff",
		tsCursor: appliedAtMs,
		seq:      1,
	}
	if _, err := f.s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms)
		 VALUES (?, ?, ?, ?, ?)`,
		f.session, f.srcAgent, f.srcSess, appliedAtMs-60_000, appliedAtMs+24*60*60*1000,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	f.loID = mkProposeRow(t, f.s, appliedAtMs-60_000)
	if err := RecordProposedSkill(context.Background(), f.s.DB(), f.loID, skill, appliedAtMs-60_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := MarkProposedSkillApplied(context.Background(), f.s.DB(), f.loID, skill,
		"/home/u/.claude/skills/"+skill+"/SKILL.md", appliedAtMs); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return f
}

// addLoad inserts a skill_load extraction at the next timestamp; if
// failureAfterMs > 0, also inserts a tool_failure event in the same
// session within that offset.
//
// Loads are spaced 15 minutes apart so each load's 10-min failure
// lookahead window is isolated — a failure attributed to one load
// won't bleed into the lookahead of an earlier load. (LoadSkillImpact
// counts a load as "failed" when ANY tool_failure falls in its
// 10-minute window, so overlapping windows cross-pollute by design.)
func (f *effectivenessFixture) addLoad(failureAfterMs int64) {
	f.t.Helper()
	f.tsCursor += 15 * 60 * 1000
	loadEvt := mkUUIDLikeID(f.t, f.skill+"-load", f.rows)
	f.rows++
	if _, err := f.s.DB().Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES (?, ?, ?, ?, ?, ?, '{}')`,
		loadEvt, f.seq, f.srcAgent, f.srcSess, f.tsCursor, f.tsCursor,
	); err != nil {
		f.t.Fatalf("envelope: %v", err)
	}
	f.seq++
	if _, err := f.s.DB().Exec(
		`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms)
		 VALUES (?, ?, ?, 'system_message', ?)`,
		loadEvt, f.session, f.srcAgent, f.tsCursor,
	); err != nil {
		f.t.Fatalf("event: %v", err)
	}
	if _, err := f.s.DB().Exec(
		`INSERT INTO extractions(event_id, session_id, kind, value)
		 VALUES (?, ?, ?, ?)`,
		loadEvt, f.session, extract.KindSkillLoad, f.skill,
	); err != nil {
		f.t.Fatalf("extraction: %v", err)
	}
	if failureAfterMs > 0 {
		failEvt := mkUUIDLikeID(f.t, f.skill+"-fail", f.rows)
		f.rows++
		failTs := f.tsCursor + failureAfterMs
		if _, err := f.s.DB().Exec(
			`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
			 VALUES (?, ?, ?, ?, ?, ?, '{}')`,
			failEvt, f.seq, f.srcAgent, f.srcSess, failTs, failTs,
		); err != nil {
			f.t.Fatalf("fail envelope: %v", err)
		}
		f.seq++
		if _, err := f.s.DB().Exec(
			`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms)
			 VALUES (?, ?, ?, ?, ?)`,
			failEvt, f.session, f.srcAgent, ingest.KindToolFailure, failTs,
		); err != nil {
			f.t.Fatalf("fail event: %v", err)
		}
	}
}

func TestLoadProposalEffectiveness_CountsLoadsAndFailures(t *testing.T) {
	t.Parallel()
	const appliedAt = int64(1_700_000_000_000)
	f := newEffectivenessFixture(t, "deploy-recipe", appliedAt)
	// 3 loads after apply: one clean, one with failure within window
	// (counts as failed), one with failure WAY later (outside window
	// → not failed).
	f.addLoad(0)              // clean
	f.addLoad(30_000)         // failure 30s after — within 10min window
	f.addLoad(20 * 60 * 1000) // failure 20min after — outside window

	out, err := LoadProposalEffectiveness(context.Background(), f.s.DB(), 0, 0, 0)
	if err != nil {
		t.Fatalf("effectiveness: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("rows: got %d want 1", len(out))
	}
	e := out[0]
	if e.LoadsAfterApply != 3 {
		t.Errorf("loads: got %d want 3", e.LoadsAfterApply)
	}
	if e.FailedLoadsAfter != 1 {
		t.Errorf("failed: got %d want 1 (only the in-window failure counts)", e.FailedLoadsAfter)
	}
	if !e.LastLoadedMs.Valid {
		t.Errorf("last_loaded_ms not populated")
	}
}

func TestLoadProposalEffectiveness_ExcludesUnapplied(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordProposedSkill(ctx, s.DB(), loID, "never-applied", 1_700_000_000_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Note: no MarkProposedSkillApplied call.
	rows, err := LoadProposalEffectiveness(ctx, s.DB(), 0, 0, 0)
	if err != nil {
		t.Fatalf("effectiveness: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows: got %d want 0 (unapplied rows must be excluded)", len(rows))
	}
}

func TestCountUnappliedProposals(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	a := mkProposeRow(t, s, 1_700_000_000_000)
	b := mkProposeRow(t, s, 1_700_000_500_000)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := RecordProposedSkill(ctx, s.DB(), a, name, 1_700_000_000_000); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	if err := RecordProposedSkill(ctx, s.DB(), b, "delta", 1_700_000_500_000); err != nil {
		t.Fatalf("record delta: %v", err)
	}
	// Apply one — leaves 3 unapplied across the two outputs.
	if err := MarkProposedSkillApplied(ctx, s.DB(), a, "alpha", "/p", 1_700_000_100_000); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got, err := CountUnappliedProposals(ctx, s.DB(), 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 3 {
		t.Errorf("count: got %d want 3", got)
	}

	// sinceMs filter — only proposals from the newer output should count.
	got, err = CountUnappliedProposals(ctx, s.DB(), 1_700_000_400_000)
	if err != nil {
		t.Fatalf("count since: %v", err)
	}
	if got != 1 {
		t.Errorf("count since: got %d want 1", got)
	}
}
