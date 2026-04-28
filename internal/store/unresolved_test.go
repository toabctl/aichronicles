package store

import (
	"strings"
	"testing"
	"time"
)

// seedSessionWithUnresolvedSummary plants a session and a summary
// row carrying the given unresolved list. ended_at offsets relative
// to "now" make the recency assertions easy to read.
func seedSessionWithUnresolvedSummary(t *testing.T, s *Store, id, cwd, topic string, endedAgo time.Duration, unresolved []string) {
	t.Helper()
	now := time.Now().UTC()
	endedAt := now.Add(-endedAgo).UnixMilli()
	startedAt := endedAt - 60*60*1000

	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms, cwd)
		 VALUES (?, 'claude-code', ?, ?, ?, ?)`,
		id, "src-"+id, startedAt, endedAt, cwd,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	var b strings.Builder
	b.WriteString(`{"topic":"` + topic + `","what_was_done":["x"],"unresolved":[`)
	for i, u := range unresolved {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + u + `"`)
	}
	b.WriteString(`],"key_files":[],"links":[]}`)
	body := b.String()
	if _, err := s.DB().Exec(
		`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms)
		 VALUES (?, 'summary', ?, 'h-'||?, 'fake-model', ?)`,
		id, body, id, endedAt,
	); err != nil {
		t.Fatalf("seed summary: %v", err)
	}
}

func TestLoadUnresolvedForCwd_ExactCwdMatchNewestFirst(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	const cwdA = "/repo/a"
	const cwdB = "/repo/b"

	// Newest in A — should appear FIRST in results.
	seedSessionWithUnresolvedSummary(t, s,
		"00000000-0000-0000-0000-0000000000a1", cwdA,
		"recent work on auth", 1*time.Hour,
		[]string{"document the new flow", "update the threat model"})

	// Older in A — should appear AFTER the newest one.
	seedSessionWithUnresolvedSummary(t, s,
		"00000000-0000-0000-0000-0000000000a2", cwdA,
		"earlier auth work", 5*24*time.Hour,
		[]string{"add tests for the redirect path"})

	// Different cwd — should NOT appear.
	seedSessionWithUnresolvedSummary(t, s,
		"00000000-0000-0000-0000-0000000000b1", cwdB,
		"unrelated", 1*time.Hour,
		[]string{"this should never surface in cwdA results"})

	got, err := LoadUnresolvedForCwd(t.Context(), s.DB(), cwdA, 0, 10, 10)
	if err != nil {
		t.Fatalf("LoadUnresolvedForCwd: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 items (2 from newest A + 1 from older A), got %d: %+v", len(got), got)
	}

	// Ordering: items from the newer session first.
	if got[0].SessionID != "00000000-0000-0000-0000-0000000000a1" {
		t.Errorf("got[0].SessionID = %q, want a1", got[0].SessionID)
	}
	if got[0].Topic != "recent work on auth" {
		t.Errorf("topic threading: %q", got[0].Topic)
	}
	if got[2].SessionID != "00000000-0000-0000-0000-0000000000a2" {
		t.Errorf("got[2].SessionID = %q, want a2", got[2].SessionID)
	}

	// Cwd filter: cwdB items must not appear.
	for _, it := range got {
		if strings.Contains(it.Item, "should never") {
			t.Errorf("cwdB item leaked: %+v", it)
		}
	}
}

func TestLoadUnresolvedForCwd_RespectsSinceCutoff(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const cwd = "/repo/x"

	// Recent (1h ago) — should be included.
	seedSessionWithUnresolvedSummary(t, s,
		"00000000-0000-0000-0000-000000000c01", cwd,
		"recent", 1*time.Hour,
		[]string{"recent unresolved"})

	// Ancient (60 days ago) — outside the default 30-day window.
	seedSessionWithUnresolvedSummary(t, s,
		"00000000-0000-0000-0000-000000000c02", cwd,
		"ancient", 60*24*time.Hour,
		[]string{"ancient unresolved"})

	got, err := LoadUnresolvedForCwd(t.Context(), s.DB(), cwd, 0, 10, 10)
	if err != nil {
		t.Fatalf("LoadUnresolvedForCwd: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 item (recent only), got %d: %+v", len(got), got)
	}
	if got[0].Item != "recent unresolved" {
		t.Errorf("wrong item kept: %+v", got[0])
	}
}

func TestLoadUnresolvedForCwd_PerSessionCap(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const cwd = "/repo/cap"

	// One session with many unresolved items.
	seedSessionWithUnresolvedSummary(t, s,
		"00000000-0000-0000-0000-0000000000ca", cwd, "lots",
		1*time.Hour,
		[]string{"u1", "u2", "u3", "u4", "u5", "u6", "u7"})

	got, err := LoadUnresolvedForCwd(t.Context(), s.DB(), cwd, 0, 10, 3)
	if err != nil {
		t.Fatalf("LoadUnresolvedForCwd: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("per-session cap not enforced: got %d, want 3", len(got))
	}
}

func TestLoadUnresolvedForCwd_SkipsEmptyAndUnparseable(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const cwd = "/repo/empty"

	// Empty unresolved list — should contribute nothing.
	seedSessionWithUnresolvedSummary(t, s,
		"00000000-0000-0000-0000-0000000000e0", cwd, "all done",
		1*time.Hour, []string{})

	// Unparseable summary body — must not crash; should be skipped.
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms, cwd)
		 VALUES ('00000000-0000-0000-0000-0000000000e1', 'claude-code', 'src-e1', ?, ?, ?)`,
		time.Now().Add(-2*time.Hour).UnixMilli(),
		time.Now().Add(-1*time.Hour).UnixMilli(),
		cwd,
	); err != nil {
		t.Fatalf("seed e1 session: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms)
		 VALUES ('00000000-0000-0000-0000-0000000000e1', 'summary', 'not json', 'h-e1', 'fake', ?)`,
		time.Now().Add(-1*time.Hour).UnixMilli(),
	); err != nil {
		t.Fatalf("seed e1 summary: %v", err)
	}

	got, err := LoadUnresolvedForCwd(t.Context(), s.DB(), cwd, 0, 10, 10)
	if err != nil {
		t.Fatalf("LoadUnresolvedForCwd: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 items, got %+v", got)
	}
}

func TestLoadUnresolvedForCwd_TrimsWhitespaceAndDropsBlank(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const cwd = "/repo/ws"

	seedSessionWithUnresolvedSummary(t, s,
		"00000000-0000-0000-0000-0000000000ws", cwd, "ws",
		1*time.Hour,
		[]string{"   trimmed  ", "", "    "})

	got, err := LoadUnresolvedForCwd(t.Context(), s.DB(), cwd, 0, 10, 10)
	if err != nil {
		t.Fatalf("LoadUnresolvedForCwd: %v", err)
	}
	if len(got) != 1 || got[0].Item != "trimmed" {
		t.Errorf("expected one trimmed item, got %+v", got)
	}
}
