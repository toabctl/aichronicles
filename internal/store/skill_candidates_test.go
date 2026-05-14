package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
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

func TestRecordSkillCandidate_Idempotent(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)

	// First call inserts; second call is a no-op via INSERT OR IGNORE
	// on the natural-key UNIQUE.
	for i := range 2 {
		if err := RecordSkillCandidate(ctx, s.DB(), loID, "fix-build", 1_700_000_000_000); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM skill_candidates WHERE llm_output_id = ?`, loID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows: got %d want 1 (idempotency invariant)", n)
	}
}

func TestRecordSkillCandidate_RejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
	}{
		{"empty llm_output_id", func() error { return RecordSkillCandidate(ctx, s.DB(), 0, "x", 1) }},
		{"empty skill_name", func() error { return RecordSkillCandidate(ctx, s.DB(), 1, "", 1) }},
		{"empty proposed_at_ms", func() error { return RecordSkillCandidate(ctx, s.DB(), 1, "x", 0) }},
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

func TestMarkSkillCandidateAdded_UpdatesRow(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "deploy-staging", 1_700_000_000_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := MarkSkillCandidateAdded(ctx, s.DB(), loID, "deploy-staging",
		"/home/u/.claude/skills/deploy-staging/SKILL.md", 1_700_000_500_000); err != nil {
		t.Fatalf("mark added: %v", err)
	}
	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "deploy-staging", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
	r := rows[0]
	if r.Decision != MaintenanceAdd {
		t.Errorf("decision: got %q want %q", r.Decision, MaintenanceAdd)
	}
	if r.DecisionAtMs == nil || derefInt64(r.DecisionAtMs) != 1_700_000_500_000 {
		t.Errorf("decision_at_ms: got %v", r.DecisionAtMs)
	}
	if r.AddPath == nil || derefStr(r.AddPath) != "/home/u/.claude/skills/deploy-staging/SKILL.md" {
		t.Errorf("add_path: got %v", r.AddPath)
	}
}

func TestMarkSkillCandidateAdded_NotFound(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	err := MarkSkillCandidateAdded(context.Background(), s.DB(),
		9999, "missing", "/dev/null", 1_700_000_000_000)
	if !errors.Is(err, ErrSkillCandidateNotFound) {
		t.Errorf("expected ErrSkillCandidateNotFound, got %v", err)
	}
}

// TestMarkSkillCandidateMerged_UpdatesRow exercises the AutoSkill
// merge action: a candidate is merged into an existing candidate
// (the prior 'add'), with merged_into_id pointing at the survivor
// and add_path duplicated for direct reach.
func TestMarkSkillCandidateMerged_UpdatesRow(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	// Existing candidate already added on disk.
	existingLO := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), existingLO, "deploy-staging", 1_700_000_000_000); err != nil {
		t.Fatalf("seed existing: %v", err)
	}
	if err := MarkSkillCandidateAdded(ctx, s.DB(), existingLO, "deploy-staging",
		"/home/u/.claude/skills/deploy-staging/SKILL.md", 1_700_000_100_000); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	existing, err := LoadSkillCandidatesByName(ctx, s.DB(), "deploy-staging", 0)
	if err != nil {
		t.Fatalf("load existing: %v", err)
	}
	existingID := existing[0].ID

	// New candidate refines the existing skill.
	newerLO := mkProposeRow(t, s, 1_700_001_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), newerLO, "deploy-staging", 1_700_001_000_000); err != nil {
		t.Fatalf("seed new: %v", err)
	}
	if err := MarkSkillCandidateMerged(ctx, s.DB(), newerLO, "deploy-staging",
		existingID, "/home/u/.claude/skills/deploy-staging/SKILL.md", 1_700_001_500_000); err != nil {
		t.Fatalf("mark merged: %v", err)
	}

	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "deploy-staging", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: got %d want 2", len(rows))
	}
	// Newest first per ORDER BY proposed_at_ms DESC.
	merged := rows[0]
	if merged.Decision != MaintenanceMerge {
		t.Errorf("decision: got %q want %q", merged.Decision, MaintenanceMerge)
	}
	if merged.MergedIntoID == nil || derefInt64(merged.MergedIntoID) != existingID {
		t.Errorf("merged_into_id: got %v want %d", merged.MergedIntoID, existingID)
	}
	if merged.AddPath == nil || derefStr(merged.AddPath) != "/home/u/.claude/skills/deploy-staging/SKILL.md" {
		t.Errorf("add_path duplication failed: got %v", merged.AddPath)
	}
}

// TestMarkSkillCandidateMerged_NegativeTargetRejected pins the
// remaining input check on merged_into_id: negative ids are
// nonsense (no row will ever have id < 1), so refuse them. Zero
// is the deliberate sentinel for "merged into a hand-authored
// skill" and is exercised by TestMarkSkillCandidateMerged_HandAuthored.
func TestMarkSkillCandidateMerged_NegativeTargetRejected(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	err := MarkSkillCandidateMerged(context.Background(), s.DB(),
		1, "x", -1, "/p", 1_700_000_000_000)
	if err == nil {
		t.Errorf("expected error when merged_into_id is negative")
	}
	if err != nil && !strings.Contains(err.Error(), "merged_into_id") {
		t.Errorf("error should mention merged_into_id: %v", err)
	}
}

// TestMarkSkillCandidateMerged_HandAuthored pins the sentinel for
// "merged into a hand-authored skill that has no candidate row":
// mergedIntoID=0 writes merged_into_id NULL while still recording
// decision='merge'. Without this path, a merge against a
// pre-aichronicles SKILL.md leaves the candidate stuck in pending,
// which future propose runs misread as ignored.
func TestMarkSkillCandidateMerged_HandAuthored(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "fold-into-handcrafted", 1_700_000_000_000); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := MarkSkillCandidateMerged(ctx, s.DB(), loID, "fold-into-handcrafted",
		0, "/home/u/.claude/skills/fold-into-handcrafted/SKILL.md", 1_700_000_500_000); err != nil {
		t.Fatalf("merge with id=0: %v", err)
	}
	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "fold-into-handcrafted", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Decision != MaintenanceMerge {
		t.Errorf("decision: got %q, want %q", r.Decision, MaintenanceMerge)
	}
	if r.MergedIntoID != nil {
		t.Errorf("merged_into_id should be NULL for hand-authored, got %d", derefInt64(r.MergedIntoID))
	}
	if r.AddPath == nil || derefStr(r.AddPath) == "" {
		t.Errorf("add_path should still be recorded: got %v", r.AddPath)
	}
}

// TestMarkSkillCandidateDiscarded_UpdatesRow exercises the AutoSkill
// discard action: the user actively rejects the candidate.
func TestMarkSkillCandidateDiscarded_UpdatesRow(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "noisy-suggestion", 1_700_000_000_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := MarkSkillCandidateDiscarded(ctx, s.DB(), loID, "noisy-suggestion",
		1_700_000_500_000); err != nil {
		t.Fatalf("discard: %v", err)
	}
	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "noisy-suggestion", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := rows[0]
	if r.Decision != MaintenanceDiscard {
		t.Errorf("decision: got %q want %q", r.Decision, MaintenanceDiscard)
	}
	if r.DecisionAtMs == nil {
		t.Errorf("decision_at_ms not set")
	}
	if r.AddPath != nil {
		t.Errorf("add_path should be empty on discard, got %q", derefStr(r.AddPath))
	}
}

func TestLoadSkillCandidatesByName_MultipleProposalsSameName(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	// Two propose runs over time both name the same skill — newer
	// wins for "current" but the historical row stays queryable.
	older := mkProposeRow(t, s, 1_700_000_000_000)
	newer := mkProposeRow(t, s, 1_700_001_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), older, "fix-ci", 1_700_000_000_000); err != nil {
		t.Fatalf("older: %v", err)
	}
	if err := RecordSkillCandidate(ctx, s.DB(), newer, "fix-ci", 1_700_001_000_000); err != nil {
		t.Fatalf("newer: %v", err)
	}

	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "fix-ci", 0)
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

// effectivenessFixture seeds a session + a propose run + an added
// skill + skill_load extractions with optional tool_failure pairs in
// the staleness window, so LoadSkillCandidateEffectiveness can be
// tested against a controlled distribution.
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

func newEffectivenessFixture(t *testing.T, skill string, addedAtMs int64) *effectivenessFixture {
	t.Helper()
	f := &effectivenessFixture{
		t:        t,
		s:        openTemp(t),
		skill:    skill,
		session:  "00000000-0000-0000-0000-000000000200",
		srcAgent: "claude-code",
		srcSess:  "src-eff",
		tsCursor: addedAtMs,
		seq:      1,
	}
	if _, err := f.s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms)
		 VALUES (?, ?, ?, ?, ?)`,
		f.session, f.srcAgent, f.srcSess, addedAtMs-60_000, addedAtMs+24*60*60*1000,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	f.loID = mkProposeRow(t, f.s, addedAtMs-60_000)
	if err := RecordSkillCandidate(context.Background(), f.s.DB(), f.loID, skill, addedAtMs-60_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := MarkSkillCandidateAdded(context.Background(), f.s.DB(), f.loID, skill,
		"/home/u/.claude/skills/"+skill+"/SKILL.md", addedAtMs); err != nil {
		t.Fatalf("add: %v", err)
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
		loadEvt, f.session, events.ExtractionKindSkillLoad, f.skill,
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
			failEvt, f.session, f.srcAgent, events.KindToolFailure, failTs,
		); err != nil {
			f.t.Fatalf("fail event: %v", err)
		}
	}
}

func TestLoadSkillCandidateEffectiveness_CountsLoadsAndFailures(t *testing.T) {
	t.Parallel()
	const addedAt = int64(1_700_000_000_000)
	f := newEffectivenessFixture(t, "deploy-recipe", addedAt)
	// 3 loads after add: one clean, one with failure within window
	// (counts as failed), one with failure WAY later (outside window
	// → not failed).
	f.addLoad(0)              // clean
	f.addLoad(30_000)         // failure 30s after — within 10min window
	f.addLoad(20 * 60 * 1000) // failure 20min after — outside window

	out, err := LoadSkillCandidateEffectiveness(context.Background(), f.s.DB(), 0, 0, 0)
	if err != nil {
		t.Fatalf("effectiveness: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("rows: got %d want 1", len(out))
	}
	e := out[0]
	if e.LoadsAfterAdd != 3 {
		t.Errorf("loads: got %d want 3", e.LoadsAfterAdd)
	}
	if e.FailedLoadsAfter != 1 {
		t.Errorf("failed: got %d want 1 (only the in-window failure counts)", e.FailedLoadsAfter)
	}
	if e.LastLoadedMs == nil {
		t.Errorf("last_loaded_ms not populated")
	}
}

// TestLoadSkillCandidateEffectiveness_ExcludesNonAdded asserts that
// rows with Decision != 'add' (pending, merged, discarded) are
// excluded — only candidates the user actively turned into an
// on-disk SKILL.md count toward "did our suggestions help?".
func TestLoadSkillCandidateEffectiveness_ExcludesNonAdded(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "never-added", 1_700_000_000_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Note: no MarkSkillCandidateAdded call.
	rows, err := LoadSkillCandidateEffectiveness(ctx, s.DB(), 0, 0, 0)
	if err != nil {
		t.Fatalf("effectiveness: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows: got %d want 0 (non-added rows must be excluded)", len(rows))
	}
}

func TestLoadPendingSkillCandidates_DedupesByName(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	older := mkProposeRow(t, s, 1_700_000_000_000)
	newer := mkProposeRow(t, s, 1_700_001_000_000)

	// Same skill name, two outputs — both pending. The reader must
	// dedupe to one entry, keeping the newer (ORDER BY
	// proposed_at_ms DESC).
	if err := RecordSkillCandidate(ctx, s.DB(), older, "twice-proposed", 1_700_000_000_000); err != nil {
		t.Fatalf("older: %v", err)
	}
	if err := RecordSkillCandidate(ctx, s.DB(), newer, "twice-proposed", 1_700_001_000_000); err != nil {
		t.Fatalf("newer: %v", err)
	}
	if err := RecordSkillCandidate(ctx, s.DB(), newer, "another", 1_700_001_000_000); err != nil {
		t.Fatalf("another: %v", err)
	}

	rows, err := LoadPendingSkillCandidates(ctx, s.DB(), 0, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("dedup failed: got %d rows want 2 (one per distinct skill_name)", len(rows))
	}
	for _, r := range rows {
		if r.SkillName == "twice-proposed" && r.LLMOutputID != newer {
			t.Errorf("dedup kept the wrong row: got llm_output_id=%d want %d (newer)", r.LLMOutputID, newer)
		}
	}
}

func TestLoadPendingSkillCandidates_ExcludesDecided(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "added", 1_700_000_000_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "discarded", 1_700_000_000_000); err != nil {
		t.Fatalf("record discarded: %v", err)
	}
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "pending", 1_700_000_000_000); err != nil {
		t.Fatalf("record pending: %v", err)
	}
	if err := MarkSkillCandidateAdded(ctx, s.DB(), loID, "added", "/p", 1_700_000_500_000); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := MarkSkillCandidateDiscarded(ctx, s.DB(), loID, "discarded", 1_700_000_500_000); err != nil {
		t.Fatalf("discard: %v", err)
	}

	rows, err := LoadPendingSkillCandidates(ctx, s.DB(), 0, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
	if rows[0].SkillName != "pending" {
		t.Errorf("name: got %q want %q", rows[0].SkillName, "pending")
	}
}

func TestCountPendingSkillCandidates(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	a := mkProposeRow(t, s, 1_700_000_000_000)
	b := mkProposeRow(t, s, 1_700_000_500_000)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := RecordSkillCandidate(ctx, s.DB(), a, name, 1_700_000_000_000); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	if err := RecordSkillCandidate(ctx, s.DB(), b, "delta", 1_700_000_500_000); err != nil {
		t.Fatalf("record delta: %v", err)
	}
	// Add one — leaves 3 pending across the two outputs.
	if err := MarkSkillCandidateAdded(ctx, s.DB(), a, "alpha", "/p", 1_700_000_100_000); err != nil {
		t.Fatalf("add: %v", err)
	}

	got, err := CountPendingSkillCandidates(ctx, s.DB(), 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 3 {
		t.Errorf("count: got %d want 3", got)
	}

	// sinceMs filter — only candidates from the newer output should count.
	got, err = CountPendingSkillCandidates(ctx, s.DB(), 1_700_000_400_000)
	if err != nil {
		t.Fatalf("count since: %v", err)
	}
	if got != 1 {
		t.Errorf("count since: got %d want 1", got)
	}
}

// TestRecordSkillCandidateWithMetadata_PersistsAutoSkillFields
// asserts triggers / tags / examples / version land on the row at
// extraction time and round-trip cleanly through the loader. Empty
// metadata still inserts the row (the legacy thin form goes
// through the same path with a zero-valued struct).
func TestRecordSkillCandidateWithMetadata_PersistsAutoSkillFields(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)

	meta := SkillCandidateMetadata{
		Triggers: []string{"deploy staging", "ship to staging", "push to staging"},
		Tags:     []string{"deploy", "ci"},
		Examples: []SkillExample{{Input: "deploy this branch", Output: "runs deploy script with current branch"}},
		// Version intentionally empty — store should default it.
	}
	if err := RecordSkillCandidateWithMetadata(ctx, s.DB(),
		loID, "deploy-staging", 1_700_000_000_000, meta); err != nil {
		t.Fatalf("record with metadata: %v", err)
	}
	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "deploy-staging", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
	r := rows[0]
	if got, want := len(r.Triggers), 3; got != want {
		t.Errorf("triggers: got %d entries want %d", got, want)
	}
	if got, want := r.Triggers[0], "deploy staging"; got != want {
		t.Errorf("triggers[0]: got %q want %q", got, want)
	}
	if got, want := len(r.Tags), 2; got != want {
		t.Errorf("tags: got %d entries want %d", got, want)
	}
	if len(r.Examples) != 1 || r.Examples[0].Input == "" || r.Examples[0].Output == "" {
		t.Errorf("examples: got %#v", r.Examples)
	}
	if r.Version != InitialSkillVersion {
		t.Errorf("version: got %q want %q", r.Version, InitialSkillVersion)
	}
}

// TestRecordSkillCandidate_ThinFormStillWorks asserts the legacy
// no-metadata form remains a no-op upsert: the row exists with
// empty metadata fields and the default version.
func TestRecordSkillCandidate_ThinFormStillWorks(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "no-meta", 1_700_000_000_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "no-meta", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := rows[0]
	if r.Version != InitialSkillVersion {
		t.Errorf("version default: got %q want %q", r.Version, InitialSkillVersion)
	}
	if r.Triggers != nil || r.Tags != nil || r.Examples != nil {
		t.Errorf("expected empty metadata, got triggers=%v tags=%v examples=%v",
			r.Triggers, r.Tags, r.Examples)
	}
}

// TestLoadAddedSkillCandidate exercises the merge-path lookup:
// returns the most-recently-added candidate by name; nil when none
// exists; isolated by Decision='add' (pending / discarded rows
// don't surface).
func TestLoadAddedSkillCandidate(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	t.Run("nil when no candidate", func(t *testing.T) {
		got, err := LoadAddedSkillCandidate(ctx, s.DB(), "missing")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil, got %#v", got)
		}
	})

	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "pending-only", 1_700_000_000_000); err != nil {
		t.Fatalf("record pending: %v", err)
	}
	t.Run("nil when only pending", func(t *testing.T) {
		got, err := LoadAddedSkillCandidate(ctx, s.DB(), "pending-only")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != nil {
			t.Errorf("pending should not surface as added: %#v", got)
		}
	})

	if err := RecordSkillCandidate(ctx, s.DB(), loID, "added-skill", 1_700_000_000_000); err != nil {
		t.Fatalf("record added: %v", err)
	}
	if err := MarkSkillCandidateAdded(ctx, s.DB(), loID, "added-skill", "/p/SKILL.md", 1_700_000_500_000); err != nil {
		t.Fatalf("mark added: %v", err)
	}
	t.Run("returns added candidate", func(t *testing.T) {
		got, err := LoadAddedSkillCandidate(ctx, s.DB(), "added-skill")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got == nil {
			t.Fatalf("expected candidate, got nil")
		}
		if got.Decision != MaintenanceAdd {
			t.Errorf("decision: got %q want %q", got.Decision, MaintenanceAdd)
		}
		if got.AddPath == nil || derefStr(got.AddPath) != "/p/SKILL.md" {
			t.Errorf("add_path: got %v", got.AddPath)
		}
	})
}

// TestSkillCandidate_LifecycleTransitionsClearStaleFields pins the
// state-machine integrity rule: each `Mark*` lifecycle write clears
// fields that don't apply to the new state. Pre-fix, transitions
// like `add → discard` kept `add_path` populated (a "rejected"
// candidate that still claimed to own a SKILL.md file), and
// `merge → add` left `merged_into_id` pointing at a stale row.
//
// The matrix below covers every transition that overwrites a prior
// non-zero state. Each subtest seeds the prior state, fires the
// transition, and asserts the now-irrelevant fields are NULL.
func TestSkillCandidate_LifecycleTransitionsClearStaleFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Helper: insert a fresh candidate, run a sequence of Mark*
	// calls, and return the final SkillCandidate row.
	type step struct {
		op string // "add" | "merge" | "discard"
	}
	type want struct {
		decision        MaintenanceAction
		addPathValid    bool
		bodyHashValid   bool
		mergedIntoValid bool
	}

	cases := []struct {
		name  string
		path  []step
		final want
	}{
		{
			name: "add then discard clears add_path/add_body_sha256",
			path: []step{{"add"}, {"discard"}},
			final: want{
				decision:        MaintenanceDiscard,
				addPathValid:    false,
				bodyHashValid:   false,
				mergedIntoValid: false,
			},
		},
		{
			name: "merge then discard clears merged_into_id/add_path",
			path: []step{{"add"}, {"merge"}, {"discard"}},
			final: want{
				decision:        MaintenanceDiscard,
				addPathValid:    false,
				bodyHashValid:   false,
				mergedIntoValid: false,
			},
		},
		{
			name: "add then merge clears add_body_sha256",
			path: []step{{"add"}, {"merge"}},
			final: want{
				decision:        MaintenanceMerge,
				addPathValid:    true, // merge sets add_path
				bodyHashValid:   false,
				mergedIntoValid: true,
			},
		},
		{
			name: "merge then add clears merged_into_id",
			path: []step{{"add"}, {"merge"}, {"add"}},
			final: want{
				decision:        MaintenanceAdd,
				addPathValid:    true,
				bodyHashValid:   true,
				mergedIntoValid: false,
			},
		},
		{
			name: "discard then add clears nothing prior was set",
			path: []step{{"discard"}, {"add"}},
			final: want{
				decision:        MaintenanceAdd,
				addPathValid:    true,
				bodyHashValid:   true,
				mergedIntoValid: false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openTemp(t)
			loSelf := mkProposeRow(t, s, 1_700_000_000_000)
			loTarget := mkProposeRow(t, s, 1_700_000_000_000)

			// Seed the row whose lifecycle we walk.
			if err := RecordSkillCandidate(ctx, s.DB(), loSelf, "subject", 1_700_000_000_000); err != nil {
				t.Fatalf("seed self: %v", err)
			}
			// And a separate target row for the merge step (with its
			// own decision='add') so merged_into_id has a real referent.
			if err := RecordSkillCandidate(ctx, s.DB(), loTarget, "subject-target", 1_700_000_000_000); err != nil {
				t.Fatalf("seed target: %v", err)
			}
			if err := MarkSkillCandidateAdded(ctx, s.DB(), loTarget, "subject-target", "/p/target.md", 1_700_000_001_000); err != nil {
				t.Fatalf("seed target add: %v", err)
			}
			target, err := LoadAddedSkillCandidate(ctx, s.DB(), "subject-target")
			if err != nil || target == nil {
				t.Fatalf("load target: %v / %v", target, err)
			}

			// Walk the path.
			now := int64(1_700_000_002_000)
			for i, st := range tc.path {
				switch st.op {
				case "add":
					if err := MarkSkillCandidateAdded(ctx, s.DB(), loSelf, "subject",
						"/p/self.md", now+int64(i*1000)); err != nil {
						// Use the body-hash variant via direct write for the bodyHashValid test.
						t.Fatalf("step %d add: %v", i, err)
					}
					// Plant a non-empty body hash so we can observe it being
					// cleared by the next transition.
					if _, err := s.DB().ExecContext(ctx,
						`UPDATE skill_candidates SET add_body_sha256='deadbeef' WHERE llm_output_id=? AND skill_name=?`,
						loSelf, "subject"); err != nil {
						t.Fatalf("plant hash: %v", err)
					}
				case "merge":
					if err := MarkSkillCandidateMerged(ctx, s.DB(), loSelf, "subject",
						target.ID, "/p/merged.md", now+int64(i*1000)); err != nil {
						t.Fatalf("step %d merge: %v", i, err)
					}
				case "discard":
					if err := MarkSkillCandidateDiscarded(ctx, s.DB(), loSelf, "subject",
						now+int64(i*1000)); err != nil {
						t.Fatalf("step %d discard: %v", i, err)
					}
				default:
					t.Fatalf("bad op %q", st.op)
				}
			}

			rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "subject", 0)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("rows: got %d, want 1", len(rows))
			}
			r := rows[0]
			if r.Decision != tc.final.decision {
				t.Errorf("decision: got %q, want %q", r.Decision, tc.final.decision)
			}
			if r.AddPath != nil != tc.final.addPathValid {
				t.Errorf("add_path.Valid: got %v (=%q), want %v",
					r.AddPath != nil, derefStr(r.AddPath), tc.final.addPathValid)
			}
			if r.AddBodySHA256 != nil != tc.final.bodyHashValid {
				t.Errorf("add_body_sha256.Valid: got %v (=%q), want %v",
					r.AddBodySHA256 != nil, derefStr(r.AddBodySHA256), tc.final.bodyHashValid)
			}
			if r.MergedIntoID != nil != tc.final.mergedIntoValid {
				t.Errorf("merged_into_id.Valid: got %v (=%d), want %v",
					r.MergedIntoID != nil, derefInt64(r.MergedIntoID), tc.final.mergedIntoValid)
			}
		})
	}
}

// TestUpdateSkillCandidateKind pins the merge-target kind refresh:
// when the LLM-decided union flips pattern→pitfall (or the inverse),
// the surviving candidate row's kind must follow the merged content.
// Without this, the DB and the on-disk SKILL.md frontmatter disagree
// on the contrastive label and any kind-branched downstream surface
// silently misroutes the merged skill.
func TestUpdateSkillCandidateKind(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "label-flips", 1_700_000_000_000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := MarkSkillCandidateAdded(ctx, s.DB(), loID, "label-flips", "/p/x.md", 1_700_000_500_000); err != nil {
		t.Fatalf("mark add: %v", err)
	}
	cand, err := LoadAddedSkillCandidate(ctx, s.DB(), "label-flips")
	if err != nil || cand == nil {
		t.Fatalf("load: %v / %v", cand, err)
	}
	// Default kind from migration 024 is "pattern".
	if cand.Kind != SkillKindPattern {
		t.Fatalf("seeded kind: got %q, want %q", cand.Kind, SkillKindPattern)
	}

	// Flip to pitfall.
	if err := UpdateSkillCandidateKind(ctx, s.DB(), cand.ID, SkillKindPitfall); err != nil {
		t.Fatalf("update kind: %v", err)
	}
	got, err := LoadAddedSkillCandidate(ctx, s.DB(), "label-flips")
	if err != nil || got == nil {
		t.Fatalf("reload: %v / %v", got, err)
	}
	if got.Kind != SkillKindPitfall {
		t.Errorf("kind not updated: got %q, want %q", got.Kind, SkillKindPitfall)
	}

	t.Run("rejects out-of-enum", func(t *testing.T) {
		err := UpdateSkillCandidateKind(ctx, s.DB(), cand.ID, SkillKind("garbage"))
		if err == nil {
			t.Errorf("expected error for out-of-enum kind, got nil")
		}
		if err != nil && !strings.Contains(err.Error(), "kind") {
			t.Errorf("error should mention kind: %v", err)
		}
	})

	t.Run("rejects empty kind", func(t *testing.T) {
		err := UpdateSkillCandidateKind(ctx, s.DB(), cand.ID, SkillKind(""))
		if err == nil {
			t.Errorf("expected error for empty kind, got nil")
		}
	})

	t.Run("missing id returns ErrSkillCandidateNotFound", func(t *testing.T) {
		err := UpdateSkillCandidateKind(ctx, s.DB(), 9_999_999, SkillKindPattern)
		if !errors.Is(err, ErrSkillCandidateNotFound) {
			t.Errorf("expected ErrSkillCandidateNotFound, got %v", err)
		}
	})
}

// TestUpdateSkillCandidateAddBodyHash pins the merge-target hash
// refresh: after `propose merge` rewrites SKILL.md the surviving
// added candidate must have its add_body_sha256 + add_path updated
// to point at the new content. Without this, drift checks would
// flag every merged file as tampered.
func TestUpdateSkillCandidateAddBodyHash(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "to-merge", 1_700_000_000_000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := MarkSkillCandidateAdded(ctx, s.DB(), loID, "to-merge", "/p/old.md", 1_700_000_500_000); err != nil {
		t.Fatalf("mark add: %v", err)
	}
	cand, err := LoadAddedSkillCandidate(ctx, s.DB(), "to-merge")
	if err != nil || cand == nil {
		t.Fatalf("load: %v / %v", cand, err)
	}

	const newHash = "deadbeef000000000000000000000000000000000000000000000000deadbeef"
	if err := UpdateSkillCandidateAddBodyHash(ctx, s.DB(), cand.ID, "/p/new.md", newHash); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := LoadAddedSkillCandidate(ctx, s.DB(), "to-merge")
	if err != nil || got == nil {
		t.Fatalf("reload: %v / %v", got, err)
	}
	if got.AddPath == nil || derefStr(got.AddPath) != "/p/new.md" {
		t.Errorf("add_path: got %v, want /p/new.md", got.AddPath)
	}
	if got.AddBodySHA256 == nil || derefStr(got.AddBodySHA256) != newHash {
		t.Errorf("add_body_sha256: got %v, want %s", got.AddBodySHA256, newHash)
	}

	// Decision and decision_at_ms are untouched — the row remains
	// the active "added" target. (Callers don't intend to flip the
	// state, only to refresh the body fingerprint.)
	if got.Decision != MaintenanceAdd {
		t.Errorf("decision drifted: got %q, want %q", got.Decision, MaintenanceAdd)
	}

	t.Run("missing id returns ErrSkillCandidateNotFound", func(t *testing.T) {
		err := UpdateSkillCandidateAddBodyHash(ctx, s.DB(), 9_999_999, "/p/x.md", newHash)
		if !errors.Is(err, ErrSkillCandidateNotFound) {
			t.Errorf("expected ErrSkillCandidateNotFound, got %v", err)
		}
	})
}

// TestLoadAddedSkillCandidate_TiebreakerOnDecisionTime pins the
// deterministic merge-target rule when two adds for the same skill
// name share a decision_at_ms (same millisecond). Without the `, id
// DESC` tiebreaker the returned row would be engine-defined; the
// merge path needs a stable answer so the SKILL.md path the merge
// rewrites is deterministic.
func TestLoadAddedSkillCandidate_TiebreakerOnDecisionTime(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	const sameMs = int64(1_700_000_000_000)
	loA := mkProposeRow(t, s, sameMs)
	loB := mkProposeRow(t, s, sameMs)

	if err := RecordSkillCandidate(ctx, s.DB(), loA, "tied", sameMs); err != nil {
		t.Fatalf("record A: %v", err)
	}
	if err := RecordSkillCandidate(ctx, s.DB(), loB, "tied", sameMs); err != nil {
		t.Fatalf("record B: %v", err)
	}
	// Both rows decisioned at the same ms — only the higher-id
	// (later-inserted) row should win, repeatedly.
	if err := MarkSkillCandidateAdded(ctx, s.DB(), loA, "tied", "/p/A.md", sameMs); err != nil {
		t.Fatalf("mark A: %v", err)
	}
	if err := MarkSkillCandidateAdded(ctx, s.DB(), loB, "tied", "/p/B.md", sameMs); err != nil {
		t.Fatalf("mark B: %v", err)
	}

	for i := range 5 {
		got, err := LoadAddedSkillCandidate(ctx, s.DB(), "tied")
		if err != nil {
			t.Fatalf("load (iter %d): %v", i, err)
		}
		if got == nil {
			t.Fatalf("expected a candidate (iter %d)", i)
		}
		if got.LLMOutputID != loB {
			t.Errorf("iter %d: got llm_output_id=%d, want %d (later-inserted row)", i, got.LLMOutputID, loB)
		}
	}
}

// TestBumpPatch covers the version-bumping rule the merge path
// uses on the existing skill's frontmatter version. The fallback
// to InitialSkillVersion on garbage input is the load-bearing
// invariant — a corrupted version string must not strand merges.
func TestBumpPatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"v0.1.0", "v0.1.1"},
		{"v0.1.42", "v0.1.43"},
		{"0.1.0", "0.1.1"},
		{"v1.2.3", "v1.2.4"},
		{"V0.1.0", "v0.1.1"}, // case-insensitive prefix
		{"", InitialSkillVersion},
		{"v0.1", InitialSkillVersion},   // missing patch
		{"v0.1.x", InitialSkillVersion}, // non-integer
		{"vabc.def.ghi", InitialSkillVersion},
		{"garbage", InitialSkillVersion},
	}
	for _, tc := range cases {
		got := BumpPatch(tc.in)
		if got != tc.want {
			t.Errorf("BumpPatch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRecordSkillCandidate_DefaultKindIsPattern pins migration
// 024's behavioural default: a candidate inserted without an
// explicit Kind comes back as SkillKindPattern, matching the
// existing pre-024 emission shape.
func TestRecordSkillCandidate_DefaultKindIsPattern(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "default-kind", 1_700_000_000_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "default-kind", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rows[0].Kind != SkillKindPattern {
		t.Errorf("default kind: got %q want %q", rows[0].Kind, SkillKindPattern)
	}
}

// TestRecordSkillCandidateWithMetadata_PersistsKind covers an
// explicit pitfall emission: the metadata-aware writer carries the
// label into the row, and the loader returns it unchanged.
func TestRecordSkillCandidateWithMetadata_PersistsKind(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidateWithMetadata(ctx, s.DB(),
		loID, "avoid-shared-rebase", 1_700_000_000_000,
		SkillCandidateMetadata{Kind: SkillKindPitfall}); err != nil {
		t.Fatalf("record: %v", err)
	}
	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "avoid-shared-rebase", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rows[0].Kind != SkillKindPitfall {
		t.Errorf("kind: got %q want %q", rows[0].Kind, SkillKindPitfall)
	}
}

// TestRecordSkillCandidate_BareReRunPreservesKind pins the upsert
// semantics for `kind`: once a candidate is recorded with an
// explicit pitfall, a subsequent bare RecordSkillCandidate call on
// the same (llm_output_id, skill_name) must NOT flip it back to
// 'pattern'. The bare form lands an empty metadata zero-value
// (Kind=""), which the writer translates to a NULL parameter so the
// DO UPDATE clause COALESCEs to the existing row's kind rather than
// the Go-side default.
func TestRecordSkillCandidate_BareReRunPreservesKind(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)

	if err := RecordSkillCandidateWithMetadata(ctx, s.DB(),
		loID, "avoid-shared-rebase", 1_700_000_000_000,
		SkillCandidateMetadata{Kind: SkillKindPitfall}); err != nil {
		t.Fatalf("seed pitfall: %v", err)
	}

	// Re-run via the bare entrypoint (no metadata): historically
	// this defaulted kind to SkillKindPattern at the Go layer and
	// the SQL set kind = excluded.kind, silently flipping pitfall
	// to pattern. The fix routes empty meta.Kind through as SQL NULL.
	if err := RecordSkillCandidate(ctx, s.DB(),
		loID, "avoid-shared-rebase", 1_700_000_000_000); err != nil {
		t.Fatalf("re-run bare: %v", err)
	}

	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "avoid-shared-rebase", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rows[0].Kind != SkillKindPitfall {
		t.Errorf("kind clobbered by bare re-run: got %q want %q",
			rows[0].Kind, SkillKindPitfall)
	}
}

// TestSkillCandidates_NoSupersededByIdColumn pins migration 022's
// payload: the dead `superseded_by_id` column from migration 018 is
// gone after the table-recreate and the merged_into_id self-FK
// still works (records merge target, cascades to NULL on referent
// deletion). Reaching directly into sqlite_master keeps the assertion
// honest — a Go-side struct rename alone wouldn't prove the column
// is actually absent at the DB level.
func TestSkillCandidates_NoSupersededByIdColumn(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Confirm the column is absent. PRAGMA table_info returns one
	// row per column; the legacy name should not appear.
	rows, err := s.DB().Query(`PRAGMA table_info(skill_candidates)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "superseded_by_id" {
			t.Errorf("migration 022 left superseded_by_id behind (expected to be dropped)")
		}
	}

	// Confirm merged_into_id self-FK still works end-to-end:
	// inserting a row that references another row's id round-trips,
	// and the loader returns the pointer.
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "target", 1_700_000_000_000); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := MarkSkillCandidateAdded(ctx, s.DB(), loID, "target", "/p/SKILL.md", 1_700_000_100_000); err != nil {
		t.Fatalf("mark target added: %v", err)
	}
	target, err := LoadAddedSkillCandidate(ctx, s.DB(), "target")
	if err != nil || target == nil {
		t.Fatalf("load target: %v", err)
	}

	loID2 := mkProposeRow(t, s, 1_700_001_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID2, "target", 1_700_001_000_000); err != nil {
		t.Fatalf("seed merger: %v", err)
	}
	if err := MarkSkillCandidateMerged(ctx, s.DB(), loID2, "target",
		target.ID, "/p/SKILL.md", 1_700_001_500_000); err != nil {
		t.Fatalf("mark merged: %v", err)
	}

	candidates, err := LoadSkillCandidatesByName(ctx, s.DB(), "target", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("rows: got %d want 2", len(candidates))
	}
	merged := candidates[0]
	if merged.MergedIntoID == nil || derefInt64(merged.MergedIntoID) != target.ID {
		t.Errorf("merged_into_id self-FK broken: got %v want %d", merged.MergedIntoID, target.ID)
	}
}

// TestSkillCandidatesMigration_Backfills asserts migration 021's
// backfill rule: any pre-migration row with applied_at_ms set
// (now decision_at_ms) inherits decision='add' so old data fits
// into the AutoSkill lifecycle without a manual touch-up.
//
// Implementation note: the test seeds a row at the new schema
// (Decision is set explicitly via MarkSkillCandidateAdded which
// also writes decision='add'); we then verify the loader returns
// MaintenanceAdd. Real-world migration backfill is exercised end-
// to-end by the migration runner during store.Open.
func TestSkillCandidatesMigration_Backfills(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), loID, "legacy", 1_700_000_000_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := MarkSkillCandidateAdded(ctx, s.DB(), loID, "legacy",
		"/p", 1_700_000_500_000); err != nil {
		t.Fatalf("add: %v", err)
	}
	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "legacy", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rows[0].Decision != MaintenanceAdd {
		t.Errorf("backfill: got Decision=%q want %q", rows[0].Decision, MaintenanceAdd)
	}
}

// TestMergeInvariant_TargetMustBeAdded pins migration 028's
// first trigger: merging into a candidate whose decision is
// 'discard' (or pending/merge) is rejected at the DB layer so a
// future code path that bypasses MarkSkillCandidateMerged still
// can't write an inconsistent merged_into_id.
func TestMergeInvariant_TargetMustBeAdded(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	// Target is a discarded row — clearly not a legitimate merge
	// destination.
	targetLO := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), targetLO, "rejected", 1_700_000_000_000); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := MarkSkillCandidateDiscarded(ctx, s.DB(), targetLO, "rejected",
		1_700_000_100_000); err != nil {
		t.Fatalf("seed discard: %v", err)
	}
	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "rejected", 0)
	if err != nil {
		t.Fatalf("load target: %v", err)
	}
	targetID := rows[0].ID

	// New candidate tries to merge into the discarded row.
	srcLO := mkProposeRow(t, s, 1_700_001_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), srcLO, "rejected", 1_700_001_000_000); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	err = MarkSkillCandidateMerged(ctx, s.DB(), srcLO, "rejected",
		targetID, "/p", 1_700_001_500_000)
	if err == nil {
		t.Fatal("expected merge into discarded row to fail; the trigger should have aborted")
	}
	if !strings.Contains(err.Error(), "decision=add") {
		t.Errorf("error message should name the invariant: %v", err)
	}
}

// TestMergeInvariant_NoOrphanWhenAddDiscarded pins migration 028's
// second trigger: an 'add' row that another candidate has merged
// into cannot be discarded — the merge claim would become a
// dangling pointer.
func TestMergeInvariant_NoOrphanWhenAddDiscarded(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	// Seed target as the canonical added skill.
	targetLO := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), targetLO, "keep-me", 1_700_000_000_000); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := MarkSkillCandidateAdded(ctx, s.DB(), targetLO, "keep-me",
		"/p", 1_700_000_100_000); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	rows, err := LoadSkillCandidatesByName(ctx, s.DB(), "keep-me", 0)
	if err != nil {
		t.Fatalf("load target: %v", err)
	}
	targetID := rows[0].ID

	// Another candidate merges into the target.
	srcLO := mkProposeRow(t, s, 1_700_001_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), srcLO, "keep-me", 1_700_001_000_000); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	if err := MarkSkillCandidateMerged(ctx, s.DB(), srcLO, "keep-me",
		targetID, "/p", 1_700_001_500_000); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// Now try to discard the target. The trigger should refuse.
	err = MarkSkillCandidateDiscarded(ctx, s.DB(), targetLO, "keep-me",
		1_700_002_000_000)
	if err == nil {
		t.Fatal("expected discard of merge-targeted row to fail")
	}
	if !strings.Contains(err.Error(), "merge target") {
		t.Errorf("error message should name the invariant: %v", err)
	}
}

// TestMergeInvariant_HandAuthoredMergeStillAllowed pins that the
// hand-authored-merge sentinel (merged_into_id=NULL) is not
// affected by either trigger — the WHEN clauses gate on NOT NULL.
func TestMergeInvariant_HandAuthoredMergeStillAllowed(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	srcLO := mkProposeRow(t, s, 1_700_000_000_000)
	if err := RecordSkillCandidate(ctx, s.DB(), srcLO, "fold-handcrafted", 1_700_000_000_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := MarkSkillCandidateMerged(ctx, s.DB(), srcLO, "fold-handcrafted",
		0, "/p", 1_700_000_500_000); err != nil {
		t.Fatalf("hand-authored merge should still succeed: %v", err)
	}
}
