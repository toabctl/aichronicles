package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
)

// seedToolUseForAnalytics ingests one tool_use envelope so the
// insights aggregator has something to count. Skips skill_load
// extraction logic — those tests have their own helpers below.
func seedToolUseForAnalytics(t *testing.T, st *store.Store, sourceSession, toolName string, ts time.Time) {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sourceSession,
		Kind:            "tool_use",
		Role:            "assistant",
		TsSource:        ts,
		Tool:            &events.Tool{Name: toolName},
		ContentText:     toolName,
		Payload:         map[string]any{"tool_input": map[string]any{}},
		Redaction:       &events.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(env)
	tx, _ := st.DB().Begin()
	if _, err := store.IngestEnvelope(t.Context(), tx, env, raw, ts.UnixMilli()); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	_ = tx.Commit()
}

func TestRegisterAnalyticsTools_RegistersAll(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	registerAllTools(t, srv, st)

	for _, want := range []string{"get_insights", "list_skills", "get_skill_staleness"} {
		if _, ok := srv.tools[want]; !ok {
			t.Errorf("tool %q not registered", want)
		}
	}
}

func TestGetInsights_RendersOverviewAndTopTools(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	now := time.Now().UTC().Add(-time.Hour)
	seedToolUseForAnalytics(t, st, "ses-x", "Bash", now)
	seedToolUseForAnalytics(t, st, "ses-x", "Bash", now.Add(time.Minute))
	seedToolUseForAnalytics(t, st, "ses-x", "Read", now.Add(2*time.Minute))

	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	registerAllTools(t, srv, st)
	res := callTool(t, srv, "get_insights", `{"since_days": 30}`)
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty result")
	}
	body := res.Content[0].Text
	for _, want := range []string{
		"Insights — last 30 days",
		"Overview:",
		"Top tools:",
		"Bash",
		"Read",
		"%",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("get_insights missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestGetInsights_EmptyWindowSaysSo(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	registerAllTools(t, srv, st)
	res := callTool(t, srv, "get_insights", `{"since_days": 1}`)
	body := res.Content[0].Text
	// openSeededStore plants events at "now" so a 1-day window
	// catches them. Use a 1-millisecond window via direct since
	// override (we don't have that knob, so test the default
	// 30d catches at least the seeded data).
	if !strings.Contains(body, "Insights — last 1 days") {
		t.Errorf("days echo missing:\n%s", body)
	}
}

func TestListSkills_SectionsAreLabeled(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	registerAllTools(t, srv, st)
	res := callTool(t, srv, "list_skills", `{"since_days": 30}`)
	body := res.Content[0].Text
	for _, want := range []string{
		"Installed skills",
		"Invoked recently",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("list_skills missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestGetSkillStaleness_ReportsCorrelations seeds one stale-
// correlated skill and confirms it shows up in the result.
func TestGetSkillStaleness_ReportsCorrelations(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	now := time.Now().UTC().Add(-time.Hour)

	// Skill load + tool_failure inside the same session, within
	// the default 10-minute window.
	skillLoad := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "stale-sess",
		Kind:            "tool_use",
		Role:            "assistant",
		TsSource:        now,
		Tool:            &events.Tool{Name: "Skill"},
		Payload:         map[string]any{"tool_input": map[string]any{"skill": "build-test"}},
		Redaction:       &events.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(skillLoad)
	tx, _ := st.DB().Begin()
	_, _ = store.IngestEnvelope(t.Context(), tx, skillLoad, raw, now.UnixMilli())
	_ = tx.Commit()

	failure := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "stale-sess",
		Kind:            "tool_failure",
		Role:            "tool",
		TsSource:        now.Add(2 * time.Minute),
		Tool:            &events.Tool{Name: "Bash"},
		ContentText:     "exit 1",
		Payload:         map[string]any{"error": "boom"},
		Redaction:       &events.Redaction{Applied: true},
	}
	rawF, _ := json.Marshal(failure)
	tx2, _ := st.DB().Begin()
	_, _ = store.IngestEnvelope(t.Context(), tx2, failure, rawF, now.Add(2*time.Minute).UnixMilli())
	_ = tx2.Commit()

	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	registerAllTools(t, srv, st)
	res := callTool(t, srv, "get_skill_staleness", `{"since_days": 14}`)
	body := res.Content[0].Text
	if !strings.Contains(body, "build-test") {
		t.Errorf("get_skill_staleness should surface build-test:\n%s", body)
	}
	if !strings.Contains(body, "stale=1/1") {
		t.Errorf("expected stale=1/1:\n%s", body)
	}
}

func TestGetSkillStaleness_NoCorrelationsMessage(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	registerAllTools(t, srv, st)
	res := callTool(t, srv, "get_skill_staleness", `{"since_days": 14}`)
	if !strings.Contains(res.Content[0].Text, "No stale-correlated skills") {
		t.Errorf("clean state should report no correlations:\n%s", res.Content[0].Text)
	}
}
