package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/ingest/extract"
)

// outcomeFixture seeds one session with a controlled sequence of
// events / extractions. Each test composes its own fixture; helper
// keeps the SQL in one place.
type outcomeFixture struct {
	t         *testing.T
	s         *Store
	session   string
	srcAgent  string
	srcSess   string
	tsCursor  int64
	seqCursor int64
	rowCursor int
}

func newOutcomeFixture(t *testing.T, sessionID string) *outcomeFixture {
	t.Helper()
	f := &outcomeFixture{
		t:         t,
		s:         openTemp(t),
		session:   sessionID,
		srcAgent:  "claude-code",
		srcSess:   "src-" + sessionID,
		tsCursor:  1_700_000_000_000,
		seqCursor: 1,
	}
	if _, err := f.s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms)
		 VALUES (?, ?, ?, ?, ?)`,
		f.session, f.srcAgent, f.srcSess, f.tsCursor, f.tsCursor+60_000,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return f
}

// addEvent inserts a raw_envelopes + events row pair at the next
// timestamp slot. Returns the event_id so callers can attach
// extractions if they want.
func (f *outcomeFixture) addEvent(kind, content string) string {
	f.t.Helper()
	f.tsCursor += 1000
	f.rowCursor++
	eventID := mkUUIDLikeID(f.t, "evt", f.rowCursor)
	if _, err := f.s.DB().Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES (?, ?, ?, ?, ?, ?, '{}')`,
		eventID, f.seqCursor, f.srcAgent, f.srcSess, f.tsCursor, f.tsCursor,
	); err != nil {
		f.t.Fatalf("raw_envelopes: %v", err)
	}
	f.seqCursor++
	if _, err := f.s.DB().Exec(
		`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, content_text)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		eventID, f.session, f.srcAgent, kind, f.tsCursor, content,
	); err != nil {
		f.t.Fatalf("events: %v", err)
	}
	return eventID
}

// addShell inserts a tool_use event + a kind=shell_command extraction
// carrying the supplied command line. Models the canonical Bash
// extractor output.
func (f *outcomeFixture) addShell(cmd string) {
	f.t.Helper()
	eventID := f.addEvent(ingest.KindToolUse, "")
	if _, err := f.s.DB().Exec(
		`INSERT INTO extractions(event_id, session_id, kind, value)
		 VALUES (?, ?, ?, ?)`,
		eventID, f.session, extract.KindShellCommand, cmd,
	); err != nil {
		f.t.Fatalf("shell extraction: %v", err)
	}
}

func TestComputeSessionOutcome_SuccessLikely(t *testing.T) {
	t.Parallel()
	f := newOutcomeFixture(t, "00000000-0000-0000-0000-000000000010")
	f.addEvent(ingest.KindUserPrompt, "fix the lint error")
	f.addEvent(ingest.KindToolUse, "")
	f.addEvent(ingest.KindToolResult, "")
	f.addEvent(ingest.KindAssistantMessage, "done")

	o, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if o.Outcome != OutcomeSuccessLikely {
		t.Errorf("outcome: got %q want %q", o.Outcome, OutcomeSuccessLikely)
	}
	if o.UserPromptCount != 1 || o.ToolUseCount != 1 || o.ToolFailureCount != 0 {
		t.Errorf("counts wrong: %+v", o)
	}
	if o.LastEventKind.Valid && o.LastEventKind.String != ingest.KindAssistantMessage {
		t.Errorf("last_event_kind: got %q want %q", o.LastEventKind.String, ingest.KindAssistantMessage)
	}
}

func TestComputeSessionOutcome_FailureLikelyByToolFailures(t *testing.T) {
	t.Parallel()
	f := newOutcomeFixture(t, "00000000-0000-0000-0000-000000000011")
	f.addEvent(ingest.KindUserPrompt, "run the migration")
	f.addEvent(ingest.KindToolUse, "")
	f.addEvent(ingest.KindToolFailure, "schema_version conflict")
	f.addEvent(ingest.KindToolUse, "")
	f.addEvent(ingest.KindToolFailure, "schema_version conflict")
	f.addEvent(ingest.KindToolUse, "")
	f.addEvent(ingest.KindToolFailure, "schema_version conflict")

	o, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if o.Outcome != OutcomeFailureLikely {
		t.Errorf("outcome: got %q want %q", o.Outcome, OutcomeFailureLikely)
	}
	if o.ToolFailureCount != 3 {
		t.Errorf("tool_failure_count: got %d want 3", o.ToolFailureCount)
	}
}

// TestComputeSessionOutcome_ToolFailureFloorScalesWithSession encodes
// the rate-aware failure threshold: small sessions still trip on 3
// failures, but a long session with rare hiccups does not. Without
// this gate, a 200-tool-use session with 3 stray failures (1.5%) was
// labelled failure_likely identically to a 5-tool session with 3 of
// 5 failing (60%) — the latter is broken, the former is normal noise.
func TestComputeSessionOutcome_ToolFailureFloorScalesWithSession(t *testing.T) {
	t.Parallel()
	t.Run("absolute threshold", func(t *testing.T) {
		// 5 attempts (3 successful tool uses, 3 failures) — small
		// sample, hitting the 3-floor still labels failure_likely.
		// Note: attempts here is 6, not 5 (3+3), well under the
		// 30-floor cutoff so the absolute rule applies.
		want := 3
		if got := toolFailureFloor(3, 3); got != want {
			t.Errorf("toolFailureFloor(3, 3) = %d, want %d", got, want)
		}
	})
	t.Run("scales linearly above 30 attempts", func(t *testing.T) {
		// 100 successful + 10 failed = 110 attempts → floor 11.
		// 200 successful + 5 failed = 205 attempts → floor 20.
		if got := toolFailureFloor(100, 10); got != 11 {
			t.Errorf("toolFailureFloor(100, 10) = %d, want 11", got)
		}
		if got := toolFailureFloor(200, 5); got != 20 {
			t.Errorf("toolFailureFloor(200, 5) = %d, want 20", got)
		}
	})
	t.Run("3 failures in 200-tool session is no longer failure_likely",
		func(t *testing.T) {
			f := newOutcomeFixture(t, "00000000-0000-0000-0000-000000000111")
			f.addEvent(ingest.KindUserPrompt, "long session")
			for range 197 {
				f.addEvent(ingest.KindToolUse, "")
			}
			for range 3 {
				f.addEvent(ingest.KindToolFailure, "transient")
			}
			f.addEvent(ingest.KindAssistantMessage, "done")
			o, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
			if err != nil {
				t.Fatalf("compute: %v", err)
			}
			// 200 attempts → floor 20; 3 failures fall short.
			if o.Outcome == OutcomeFailureLikely {
				t.Errorf("expected non-failure for 3/200 (1.5%%); got %q", o.Outcome)
			}
			if o.ToolFailureCount != 3 {
				t.Errorf("tool_failure_count: got %d want 3", o.ToolFailureCount)
			}
		})
	t.Run("25 failures in 200-tool session is failure_likely",
		func(t *testing.T) {
			f := newOutcomeFixture(t, "00000000-0000-0000-0000-000000000112")
			f.addEvent(ingest.KindUserPrompt, "really broken session")
			for range 175 {
				f.addEvent(ingest.KindToolUse, "")
			}
			for range 25 {
				f.addEvent(ingest.KindToolFailure, "broke")
			}
			f.addEvent(ingest.KindAssistantMessage, "giving up")
			o, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
			if err != nil {
				t.Fatalf("compute: %v", err)
			}
			if o.Outcome != OutcomeFailureLikely {
				t.Errorf("expected failure_likely for 25/200 (12.5%%); got %q", o.Outcome)
			}
		})
}

func TestComputeSessionOutcome_FailureLikelyByGitUndo(t *testing.T) {
	t.Parallel()
	f := newOutcomeFixture(t, "00000000-0000-0000-0000-000000000012")
	f.addEvent(ingest.KindUserPrompt, "tweak the config")
	f.addShell("git status")
	f.addShell("vim config.toml") // benign
	f.addShell("git reset --hard HEAD")

	o, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if o.Outcome != OutcomeFailureLikely {
		t.Errorf("outcome: got %q want %q (counts=%+v)", o.Outcome, OutcomeFailureLikely, o)
	}
	if o.GitUndoCount != 1 {
		t.Errorf("git_undo_count: got %d want 1", o.GitUndoCount)
	}
}

func TestComputeSessionOutcome_GitUndoConservative(t *testing.T) {
	t.Parallel()
	// "git reset HEAD" (just unstaging) MUST NOT count.
	// "git checkout main" (branch switch, no path) MUST NOT count.
	// "git reset --hard" inside a chain MUST count.
	f := newOutcomeFixture(t, "00000000-0000-0000-0000-000000000013")
	f.addEvent(ingest.KindUserPrompt, "branch hop")
	f.addShell("git reset HEAD")
	f.addShell("git checkout main")
	f.addShell("cd repo && git reset --hard origin/main")
	f.addShell("git checkout -- broken.go")
	f.addShell("git stash")
	f.addShell("git revert abc123")

	o, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	// Three undos: chained reset --hard, checkout --, stash, revert
	// = 4 actually. Let me count: chained reset --hard (yes), checkout
	// -- broken.go (yes), git stash (yes), git revert (yes) = 4.
	// The first two (`git reset HEAD`, `git checkout main`) are
	// excluded by gitUndoRE.
	if o.GitUndoCount != 4 {
		t.Errorf("git_undo_count: got %d want 4 (counts=%+v)", o.GitUndoCount, o)
	}
}

func TestComputeSessionOutcome_FailureLikelyByPromptRepeat(t *testing.T) {
	t.Parallel()
	f := newOutcomeFixture(t, "00000000-0000-0000-0000-000000000014")
	// Same prompt three times in a row → 2 repeats (n-1 for run length n).
	f.addEvent(ingest.KindUserPrompt, "fix the build")
	f.addEvent(ingest.KindAssistantMessage, "trying...")
	f.addEvent(ingest.KindUserPrompt, "  Fix THE Build")
	f.addEvent(ingest.KindAssistantMessage, "still trying...")
	f.addEvent(ingest.KindUserPrompt, "fix the\tbuild")

	o, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if o.PromptRepeatCount != 2 {
		t.Errorf("prompt_repeat_count: got %d want 2", o.PromptRepeatCount)
	}
	if o.Outcome != OutcomeFailureLikely {
		t.Errorf("outcome: got %q want %q", o.Outcome, OutcomeFailureLikely)
	}
}

func TestComputeSessionOutcome_FailureLikelyByEndedOnFailure(t *testing.T) {
	t.Parallel()
	f := newOutcomeFixture(t, "00000000-0000-0000-0000-000000000015")
	f.addEvent(ingest.KindUserPrompt, "deploy")
	f.addEvent(ingest.KindToolUse, "")
	f.addEvent(ingest.KindToolFailure, "connection refused")

	o, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	// One tool_failure on its own doesn't cross the >=3 bar, but
	// session ended on failure with at least one failure recorded —
	// the "ended-on-failure" rule trips.
	if o.Outcome != OutcomeFailureLikely {
		t.Errorf("outcome: got %q want %q (counts=%+v)", o.Outcome, OutcomeFailureLikely, o)
	}
	if !o.LastEventKind.Valid || o.LastEventKind.String != ingest.KindToolFailure {
		t.Errorf("last_event_kind: got %v want %q", o.LastEventKind, ingest.KindToolFailure)
	}
}

func TestComputeSessionOutcome_Mixed(t *testing.T) {
	t.Parallel()
	f := newOutcomeFixture(t, "00000000-0000-0000-0000-000000000016")
	f.addEvent(ingest.KindUserPrompt, "draft a fix")
	f.addEvent(ingest.KindToolUse, "")
	f.addEvent(ingest.KindToolFailure, "transient flake") // single failure, no other markers
	f.addEvent(ingest.KindToolUse, "")
	f.addEvent(ingest.KindAssistantMessage, "retried, ok")

	o, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	// One failure, recovered: not failure (count<3, not ended on
	// failure), not success (failures != 0), so mixed.
	if o.Outcome != OutcomeMixed {
		t.Errorf("outcome: got %q want %q (counts=%+v)", o.Outcome, OutcomeMixed, o)
	}
}

func TestComputeSessionOutcome_Unknown(t *testing.T) {
	t.Parallel()
	f := newOutcomeFixture(t, "00000000-0000-0000-0000-000000000017")
	// One prompt, no tool use — too thin to label.
	f.addEvent(ingest.KindUserPrompt, "/loop")

	o, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if o.Outcome != OutcomeUnknown {
		t.Errorf("outcome: got %q want %q (counts=%+v)", o.Outcome, OutcomeUnknown, o)
	}
}

func TestComputeSessionOutcome_SessionNotFound(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	_, err := ComputeSessionOutcome(context.Background(), s.DB(), "00000000-0000-0000-0000-000000000099")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestSaveAndLoadSessionOutcome_Roundtrip(t *testing.T) {
	t.Parallel()
	f := newOutcomeFixture(t, "00000000-0000-0000-0000-000000000018")
	f.addEvent(ingest.KindUserPrompt, "do the thing")
	f.addEvent(ingest.KindToolUse, "")
	f.addEvent(ingest.KindAssistantMessage, "done")

	o, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if err := SaveSessionOutcome(context.Background(), f.s.DB(), o); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded == nil {
		t.Fatalf("LoadSessionOutcome returned nil for saved row")
	}
	if loaded.Outcome != o.Outcome {
		t.Errorf("outcome roundtrip: got %q want %q", loaded.Outcome, o.Outcome)
	}
	if loaded.UserPromptCount != o.UserPromptCount ||
		loaded.ToolUseCount != o.ToolUseCount ||
		loaded.ToolFailureCount != o.ToolFailureCount {
		t.Errorf("count roundtrip wrong: got %+v want %+v", loaded, o)
	}
}

func TestSaveSessionOutcome_RecomputeOverwrites(t *testing.T) {
	t.Parallel()
	f := newOutcomeFixture(t, "00000000-0000-0000-0000-000000000019")
	f.addEvent(ingest.KindUserPrompt, "first prompt")
	f.addEvent(ingest.KindToolUse, "")

	first, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("compute v1: %v", err)
	}
	if err := SaveSessionOutcome(context.Background(), f.s.DB(), first); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	// Add more events and recompute — outcome label may change.
	f.addEvent(ingest.KindUserPrompt, "second prompt")
	f.addEvent(ingest.KindToolFailure, "broke")
	f.addEvent(ingest.KindToolFailure, "broke")
	f.addEvent(ingest.KindToolFailure, "broke")

	second, err := ComputeSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("compute v2: %v", err)
	}
	if err := SaveSessionOutcome(context.Background(), f.s.DB(), second); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	loaded, err := LoadSessionOutcome(context.Background(), f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded == nil {
		t.Fatalf("nil after second save")
	}
	if loaded.ToolFailureCount != 3 {
		t.Errorf("recompute lost: tool_failure_count=%d want 3", loaded.ToolFailureCount)
	}
	if loaded.Outcome != OutcomeFailureLikely {
		t.Errorf("recompute label: got %q want %q", loaded.Outcome, OutcomeFailureLikely)
	}

	// Exactly one row exists per session (PK invariant).
	var rows int
	if err := f.s.DB().QueryRow(`SELECT COUNT(*) FROM session_outcomes WHERE session_id = ?`, f.session).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("upsert violated PK: rows=%d want 1", rows)
	}
}

func TestLoadSessionOutcomes_Batch(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	// Empty input → empty map, no error, no panic.
	got, err := LoadSessionOutcomes(ctx, s.DB(), nil)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty: got %d entries want 0", len(got))
	}

	// Two sessions, one with an outcome row, one without.
	for _, id := range []string{
		"00000000-0000-0000-0000-00000000001a",
		"00000000-0000-0000-0000-00000000001b",
	} {
		if _, err := s.DB().Exec(
			`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, ?, ?)`,
			id, "claude-code", "src-"+id,
		); err != nil {
			t.Fatalf("seed session %s: %v", id, err)
		}
	}
	if err := SaveSessionOutcome(ctx, s.DB(), SessionOutcome{
		SessionID:    "00000000-0000-0000-0000-00000000001a",
		ComputedAtMs: 123,
		Outcome:      OutcomeSuccessLikely,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err = LoadSessionOutcomes(ctx, s.DB(), []string{
		"00000000-0000-0000-0000-00000000001a",
		"00000000-0000-0000-0000-00000000001b",
	})
	if err != nil {
		t.Fatalf("batch load: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("batch len: got %d want 1 (sessions without outcomes are absent)", len(got))
	}
	if _, ok := got["00000000-0000-0000-0000-00000000001a"]; !ok {
		t.Errorf("missing the session that has an outcome")
	}
}

func TestSaveSessionOutcome_RejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	if err := SaveSessionOutcome(ctx, s.DB(), SessionOutcome{Outcome: OutcomeUnknown}); err == nil {
		t.Errorf("expected error for empty session_id")
	}
	if err := SaveSessionOutcome(ctx, s.DB(), SessionOutcome{SessionID: "x"}); err == nil {
		t.Errorf("expected error for empty outcome label")
	}
}

func TestNormalizePrompt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"hello world", "hello world"},
		{"  Hello\tWorld  ", "hello world"},
		{"FIX\nthe\nbuild", "fix the build"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizePrompt(c.in); got != c.want {
			t.Errorf("normalizePrompt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLoadFailureShapes_OnlyReturnsFailureLikely(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	// Three sessions: a failure_likely, a success_likely, a mixed.
	// Only the failure_likely should appear in the result.
	specs := []struct {
		id      string
		outcome OutcomeLabel
	}{
		{"00000000-0000-0000-0000-0000000000fa", OutcomeFailureLikely},
		{"00000000-0000-0000-0000-0000000000fb", OutcomeSuccessLikely},
		{"00000000-0000-0000-0000-0000000000fc", OutcomeMixed},
	}
	now := int64(1_700_000_000_000)
	for _, sp := range specs {
		if _, err := s.DB().Exec(
			`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms, summary_topic)
			 VALUES (?, 'claude-code', ?, ?, ?, ?)`,
			sp.id, "src-"+sp.id, now-60_000, now, "topic-"+string(sp.outcome),
		); err != nil {
			t.Fatalf("seed session %s: %v", sp.id, err)
		}
		if err := SaveSessionOutcome(ctx, s.DB(), SessionOutcome{
			SessionID:        sp.id,
			ComputedAtMs:     now,
			ToolFailureCount: 5, // make sure markers render even on success_likely (no-op since success_likely won't be returned)
			Outcome:          sp.outcome,
		}); err != nil {
			t.Fatalf("seed outcome %s: %v", sp.id, err)
		}
	}

	got, err := LoadFailureShapes(ctx, s.DB(), 0, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 failure_likely row, got %d (got %+v)", len(got), got)
	}
	if got[0].SessionID != "00000000-0000-0000-0000-0000000000fa" {
		t.Errorf("got session %q, want failure_likely id", got[0].SessionID)
	}
	if got[0].ToolFailureCount != 5 {
		t.Errorf("counter not roundtripped: %+v", got[0])
	}
	if got[0].Title != "topic-failure_likely" {
		t.Errorf("title fallback wrong: %q", got[0].Title)
	}
}

func TestLoadFailureShapes_FallsBackToFirstPromptWhenNoTopic(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	const sessID = "00000000-0000-0000-0000-0000000000ff"
	now := int64(1_700_000_000_000)
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms,
		                      first_prompt_text, summary_topic)
		 VALUES (?, 'claude-code', 'src-x', ?, ?, ?, NULL)`,
		sessID, now-60_000, now, "fix the deploy that keeps failing",
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := SaveSessionOutcome(ctx, s.DB(), SessionOutcome{
		SessionID:    sessID,
		ComputedAtMs: now,
		Outcome:      OutcomeFailureLikely,
	}); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}

	got, err := LoadFailureShapes(ctx, s.DB(), 0, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got[0].Title != "fix the deploy that keeps failing" {
		t.Errorf("title should fall back to first_prompt_text when topic is empty, got %+v", got)
	}
}

func TestLoadFailureShapes_RespectsSinceAndLimit(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	for i := range 6 {
		id := fmt.Sprintf("00000000-0000-0000-0000-00000000ff%02d", i)
		ended := int64(1_700_000_000_000) + int64(i*1000)
		if _, err := s.DB().Exec(
			`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms, summary_topic)
			 VALUES (?, 'claude-code', ?, ?, ?, ?)`,
			id, "src-"+id, ended-60_000, ended, fmt.Sprintf("t%d", i),
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := SaveSessionOutcome(ctx, s.DB(), SessionOutcome{
			SessionID: id, ComputedAtMs: ended, Outcome: OutcomeFailureLikely,
		}); err != nil {
			t.Fatalf("save outcome: %v", err)
		}
	}

	// Limit=3 returns 3 newest-first.
	got, err := LoadFailureShapes(ctx, s.DB(), 0, 3)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d want 3", len(got))
	}
	if got[0].Title != "t5" {
		t.Errorf("newest-first violated: top=%q want t5", got[0].Title)
	}

	// sinceMs filter — only sessions ended at or after the cutoff.
	got, err = LoadFailureShapes(ctx, s.DB(), 1_700_000_004_000, 0)
	if err != nil {
		t.Fatalf("load (since): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("since filter: got %d want 2 (i=4 and i=5)", len(got))
	}
}

func TestEnsureSessionOutcome_ComputesAndCaches(t *testing.T) {
	t.Parallel()
	f := newOutcomeFixture(t, "00000000-0000-0000-0000-00000000001c")
	f.addEvent(ingest.KindUserPrompt, "do the thing")
	f.addEvent(ingest.KindToolUse, "")
	f.addEvent(ingest.KindAssistantMessage, "done")

	ctx := context.Background()

	// First call: no cached row, must compute and persist.
	first, err := EnsureSessionOutcome(ctx, f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("ensure (first): %v", err)
	}
	if first == nil {
		t.Fatalf("ensure returned nil on first call")
	}
	if first.Outcome != OutcomeSuccessLikely {
		t.Errorf("first outcome: got %q want %q", first.Outcome, OutcomeSuccessLikely)
	}

	// Verify a row was persisted.
	loaded, err := LoadSessionOutcome(ctx, f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("load after ensure: %v", err)
	}
	if loaded == nil {
		t.Fatalf("Ensure didn't persist a row")
	}

	// Second call: must return the cached row, no recompute. Mutate
	// the cached row's outcome label to a sentinel; if Ensure
	// recomputed, the sentinel would be overwritten.
	if _, err := f.s.DB().Exec(
		`UPDATE session_outcomes SET outcome = 'mixed' WHERE session_id = ?`,
		f.session,
	); err != nil {
		t.Fatalf("sentinel update: %v", err)
	}
	second, err := EnsureSessionOutcome(ctx, f.s.DB(), f.session)
	if err != nil {
		t.Fatalf("ensure (second): %v", err)
	}
	if second.Outcome != OutcomeMixed {
		t.Errorf("second call recomputed instead of returning cache: got %q want %q", second.Outcome, OutcomeMixed)
	}
}

func TestEnsureSessionOutcome_PropagatesNotFoundError(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	_, err := EnsureSessionOutcome(context.Background(), s.DB(), "00000000-0000-0000-0000-0000000000ff")
	if err == nil {
		t.Fatalf("expected error for unknown session id")
	}
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound wrapped, got %v", err)
	}
}

func TestSplitShellChain(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"git status", []string{"git status"}},
		{"cd repo && git reset --hard", []string{"cd repo", "git reset --hard"}},
		{"echo hi; rm -rf /tmp/foo", []string{"echo hi", "rm -rf /tmp/foo"}},
		{"true || false", []string{"true", "false"}},
		{"a && b && c", []string{"a", "b", "c"}},
	}
	for _, c := range cases {
		got := splitShellChain(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitShellChain(%q) len = %d (%q), want %d (%q)", c.in, len(got), got, len(c.want), c.want)
			continue
		}
		for i, g := range got {
			if g != c.want[i] {
				t.Errorf("splitShellChain(%q)[%d] = %q, want %q", c.in, i, g, c.want[i])
			}
		}
	}
}
