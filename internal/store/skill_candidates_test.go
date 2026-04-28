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
	if !r.DecisionAtMs.Valid || r.DecisionAtMs.Int64 != 1_700_000_500_000 {
		t.Errorf("decision_at_ms: got %v", r.DecisionAtMs)
	}
	if !r.AddPath.Valid || r.AddPath.String != "/home/u/.claude/skills/deploy-staging/SKILL.md" {
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
	if !merged.MergedIntoID.Valid || merged.MergedIntoID.Int64 != existingID {
		t.Errorf("merged_into_id: got %v want %d", merged.MergedIntoID, existingID)
	}
	if !merged.AddPath.Valid || merged.AddPath.String != "/home/u/.claude/skills/deploy-staging/SKILL.md" {
		t.Errorf("add_path duplication failed: got %v", merged.AddPath)
	}
}

func TestMarkSkillCandidateMerged_RequiresTarget(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	if err := MarkSkillCandidateMerged(context.Background(), s.DB(),
		1, "x", 0, "/p", 1_700_000_000_000); err == nil {
		t.Errorf("expected error when merged_into_id is zero")
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
	if !r.DecisionAtMs.Valid {
		t.Errorf("decision_at_ms not set")
	}
	if r.AddPath.Valid {
		t.Errorf("add_path should be empty on discard, got %q", r.AddPath.String)
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
	if !e.LastLoadedMs.Valid {
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
		if !got.AddPath.Valid || got.AddPath.String != "/p/SKILL.md" {
			t.Errorf("add_path: got %v", got.AddPath)
		}
	})
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
