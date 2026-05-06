package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
)

// seedInsightsEvents inserts a tool_use event with the given tool
// name. Used to populate the top-tools query. The exact session
// uniqueness shape doesn't matter here — every event lands in the
// same session unless caller varies sessionKey.
func seedInsightsEvents(t *testing.T, s *Store, sessionKey, toolName string, n int, baseTs time.Time) {
	t.Helper()
	for i := range n {
		env := &events.Envelope{
			V:               1,
			EventID:         uuid.Must(uuid.NewV7()).String(),
			SourceAgent:     "claude-code",
			SourceSessionID: sessionKey,
			Kind:            "tool_use",
			Role:            "assistant",
			TsSource:        baseTs.Add(time.Duration(i) * time.Minute),
			Tool:            &events.Tool{Name: toolName},
			ContentText:     toolName,
			Payload:         map[string]any{"tool_input": map[string]any{}},
			Redaction:       &events.Redaction{Applied: true},
		}
		withTx(t, s, func(tx *sql.Tx) {
			if _, err := IngestEnvelope(t.Context(), tx, env, []byte(`{"v":1}`), env.TsSource.UnixMilli()); err != nil {
				t.Fatalf("seed ingest: %v", err)
			}
		})
	}
}

// seedSkillLoad inserts one Skill tool_use envelope with the named
// skill in tool_input — exercises the skill_load extractor end-to-
// end (insert event → extractor → row in extractions).
func seedSkillLoad(t *testing.T, s *Store, sessionKey, skillName string, ts time.Time) {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sessionKey,
		Kind:            "tool_use",
		Role:            "assistant",
		TsSource:        ts,
		Tool:            &events.Tool{Name: "Skill"},
		ContentText:     "Skill",
		Payload: map[string]any{
			"tool_input": map[string]any{"skill": skillName},
		},
		Redaction: &events.Redaction{Applied: true},
	}
	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, []byte(`{"v":1}`), env.TsSource.UnixMilli()); err != nil {
			t.Fatalf("seed skill_load: %v", err)
		}
	})
}

// TestLoadInsights_EmptyWindow returns a fully-zero report rather
// than an error so callers can render "no sessions in window"
// without conditional probing.
func TestLoadInsights_EmptyWindow(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	r, err := LoadInsights(t.Context(), s.DB(),
		time.Now().Add(-24*time.Hour).UnixMilli(), InsightsLimits{})
	if err != nil {
		t.Fatalf("LoadInsights: %v", err)
	}
	if r.Overview.Sessions != 0 || r.Overview.Events != 0 {
		t.Errorf("expected zero counts, got %+v", r.Overview)
	}
	if len(r.ActivityByHour) != 24 {
		t.Errorf("ActivityByHour: got %d, want 24 dense buckets", len(r.ActivityByHour))
	}
}

// TestLoadInsights_TopToolsAndOverview pins the headline counts:
// total tool calls, distinct-tools, and the top-tools sort order
// (descending by count).
func TestLoadInsights_TopToolsAndOverview(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	base := time.Now().UTC().Add(-time.Hour)
	seedInsightsEvents(t, s, "s1", "Bash", 5, base)
	seedInsightsEvents(t, s, "s1", "Read", 3, base.Add(time.Minute))
	seedInsightsEvents(t, s, "s1", "Edit", 2, base.Add(2*time.Minute))

	since := base.Add(-24 * time.Hour).UnixMilli()
	r, err := LoadInsights(t.Context(), s.DB(), since, InsightsLimits{})
	if err != nil {
		t.Fatalf("LoadInsights: %v", err)
	}
	if r.Overview.ToolUses != 10 {
		t.Errorf("ToolUses: got %d, want 10", r.Overview.ToolUses)
	}
	if r.Overview.DistinctTools != 3 {
		t.Errorf("DistinctTools: got %d, want 3", r.Overview.DistinctTools)
	}
	if len(r.TopTools) != 3 || r.TopTools[0].ToolName != "Bash" || r.TopTools[0].Count != 5 {
		t.Errorf("TopTools[0]: got %+v, want Bash×5", r.TopTools)
	}
	if r.TopTools[1].ToolName != "Read" || r.TopTools[2].ToolName != "Edit" {
		t.Errorf("TopTools order: got %+v", r.TopTools)
	}
}

// TestLoadInsights_TopSkillsExtractedAndSorted joins the
// extractions table to the skills view and confirms the
// extractor ran (3 rows for "build-test") and that the result is
// sorted by count desc.
func TestLoadInsights_TopSkillsExtractedAndSorted(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	base := time.Now().UTC().Add(-time.Hour)
	seedSkillLoad(t, s, "s1", "build-test", base)
	seedSkillLoad(t, s, "s1", "build-test", base.Add(time.Minute))
	seedSkillLoad(t, s, "s2", "build-test", base.Add(2*time.Minute))
	seedSkillLoad(t, s, "s1", "effective-go", base.Add(3*time.Minute))

	since := base.Add(-24 * time.Hour).UnixMilli()
	r, err := LoadInsights(t.Context(), s.DB(), since, InsightsLimits{})
	if err != nil {
		t.Fatalf("LoadInsights: %v", err)
	}
	if len(r.TopSkills) != 2 {
		t.Fatalf("TopSkills: got %d rows, want 2", len(r.TopSkills))
	}
	if r.TopSkills[0].Name != "build-test" || r.TopSkills[0].Count != 3 {
		t.Errorf("TopSkills[0]: got %+v", r.TopSkills[0])
	}
	if r.TopSkills[1].Name != "effective-go" || r.TopSkills[1].Count != 1 {
		t.Errorf("TopSkills[1]: got %+v", r.TopSkills[1])
	}
	if r.Overview.DistinctSkills != 2 {
		t.Errorf("DistinctSkills: got %d, want 2", r.Overview.DistinctSkills)
	}
}

// TestLoadInsights_RespectsTopLimits confirms the caller-supplied
// caps clamp each list, so a noisy event store doesn't blow the
// report up to thousands of rows.
func TestLoadInsights_RespectsTopLimits(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	base := time.Now().UTC().Add(-time.Hour)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		seedInsightsEvents(t, s, "s1", n, 1, base)
	}
	since := base.Add(-24 * time.Hour).UnixMilli()
	r, err := LoadInsights(t.Context(), s.DB(), since, InsightsLimits{TopTools: 2})
	if err != nil {
		t.Fatalf("LoadInsights: %v", err)
	}
	if len(r.TopTools) != 2 {
		t.Errorf("TopTools cap=2: got %d rows, want 2", len(r.TopTools))
	}
}

// TestLoadInsights_ActivityByHourIsDense24 covers the contract
// that the histogram array always has 24 entries even when most
// hours are empty — the renderer expects a dense array.
func TestLoadInsights_ActivityByHourIsDense24(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	// Single event at hour 09 UTC.
	seedInsightsEvents(t, s, "s1", "Bash", 1,
		time.Date(2026, 4, 27, 9, 30, 0, 0, time.UTC))
	since := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	r, err := LoadInsights(t.Context(), s.DB(), since, InsightsLimits{})
	if err != nil {
		t.Fatalf("LoadInsights: %v", err)
	}
	if len(r.ActivityByHour) != 24 {
		t.Fatalf("expected 24 buckets, got %d", len(r.ActivityByHour))
	}
	for i, b := range r.ActivityByHour {
		if b.Hour != i {
			t.Errorf("bucket %d: hour=%d, want %d", i, b.Hour, i)
		}
	}
	if r.ActivityByHour[9].Count != 1 {
		t.Errorf("hour 9: got %d, want 1", r.ActivityByHour[9].Count)
	}
	for h := range 24 {
		if h == 9 {
			continue
		}
		if r.ActivityByHour[h].Count != 0 {
			t.Errorf("hour %d: got %d, want 0", h, r.ActivityByHour[h].Count)
		}
	}
}

// TestLoadInsights_TopSessionsByEventCount confirms sessions are
// ranked by their event_count column (which the trigger
// maintains) descending — busiest first.
func TestLoadInsights_TopSessionsByEventCount(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	base := time.Now().UTC().Add(-time.Hour)
	seedInsightsEvents(t, s, "small", "Bash", 2, base)
	seedInsightsEvents(t, s, "big", "Bash", 5, base.Add(time.Hour))

	since := base.Add(-24 * time.Hour).UnixMilli()
	r, err := LoadInsights(t.Context(), s.DB(), since, InsightsLimits{})
	if err != nil {
		t.Fatalf("LoadInsights: %v", err)
	}
	if len(r.TopSessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(r.TopSessions))
	}
	if r.TopSessions[0].EventCount < r.TopSessions[1].EventCount {
		t.Errorf("ordering: got %+v", r.TopSessions)
	}
	if r.TopSessions[0].EventCount != 5 {
		t.Errorf("top session events: got %d, want 5", r.TopSessions[0].EventCount)
	}
}
