package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/wire"
)

// renderInsightsText delegates almost everything to small format
// helpers; these tests pin the user-visible behaviour rather than
// the internals.

func TestRenderInsights_EmptyWindow(t *testing.T) {
	t.Parallel()
	r := &wire.Insights{
		Window: wire.InsightsWindow{
			SinceMs: time.Now().Add(-7 * 24 * time.Hour).UnixMilli(),
			UntilMs: time.Now().UnixMilli(),
			Days:    7,
		},
	}
	var buf bytes.Buffer
	if err := renderInsights(&buf, r, FormatTable); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, "no sessions in window") {
		t.Errorf("expected empty marker, got:\n%s", body)
	}
}

func TestRenderInsights_TextHasAllSectionsWhenPopulated(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 4, 24, 9, 0, 0, 0, time.UTC).UnixMilli()
	cwd := "/home/dev/proj"
	r := &wire.Insights{
		Window:   wire.InsightsWindow{SinceMs: 0, UntilMs: time.Now().UnixMilli(), Days: 7},
		Overview: wire.InsightsOverview{Sessions: 3, Events: 100, ToolUses: 50, UserPrompts: 5, DistinctTools: 4, DistinctSkills: 2},
		TopTools: []wire.ToolUsage{{ToolName: "Bash", Count: 30}, {ToolName: "Read", Count: 15}},
		TopSkills: []wire.SkillUsage{
			{Name: "build-test", Count: 4, LastUsedMs: time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC).UnixMilli()},
		},
		ActivityByHour: makeHourBuckets(map[int]int{9: 12, 14: 7}),
		TopSessions: []wire.TopSession{
			{
				SessionID:   "abcd1234-1111-2222-3333-444444444444",
				EventCount:  42,
				StartedAtMs: &started,
				Cwd:         &cwd,
				FirstPrompt: "investigate the slow Bash invocation under heavy load",
			},
		},
	}
	var buf bytes.Buffer
	if err := renderInsights(&buf, r, FormatTable); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	for _, want := range []string{
		"Overview",
		"sessions:        3",
		"Top tools",
		"Bash",
		"Top skills",
		"build-test",
		"Activity by hour",
		"Top sessions",
		"abcd1234",
		"/home/dev/proj",
		"investigate the slow Bash invocation",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("text render missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestRenderInsights_JSONEmitsRawStruct confirms --format=json
// streams the underlying struct so callers (web / future MCP)
// don't have to re-derive anything from text.
func TestRenderInsights_JSONEmitsRawStruct(t *testing.T) {
	t.Parallel()
	r := &wire.Insights{
		Window:   wire.InsightsWindow{SinceMs: 100, UntilMs: 200, Days: 1},
		Overview: wire.InsightsOverview{Sessions: 1, Events: 10, ToolUses: 5, DistinctTools: 1},
		TopTools: []wire.ToolUsage{{ToolName: "Bash", Count: 5}},
	}
	var buf bytes.Buffer
	if err := renderInsights(&buf, r, FormatJSON); err != nil {
		t.Fatalf("render: %v", err)
	}
	var got wire.Insights
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json roundtrip: %v\nbody: %s", err, buf.String())
	}
	if got.Overview.Sessions != 1 || got.TopTools[0].ToolName != "Bash" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// TestRenderInsightsText_OmitsEmptySections pins the no-section
// rule: when a slice is empty its header is suppressed entirely
// (no "Top Skills:" with zero rows below).
func TestRenderInsightsText_OmitsEmptySections(t *testing.T) {
	t.Parallel()
	r := &wire.Insights{
		Window:         wire.InsightsWindow{Days: 7},
		Overview:       wire.InsightsOverview{Sessions: 1, Events: 1, ToolUses: 0},
		ActivityByHour: makeHourBuckets(nil),
	}
	var buf bytes.Buffer
	if err := renderInsights(&buf, r, FormatTable); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	for _, unwanted := range []string{
		"Top tools",
		"Top skills",
		"Activity by hour",
		"Top sessions",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("empty report leaked %q section:\n%s", unwanted, body)
		}
	}
}

// makeHourBuckets builds a dense 24-bucket array with the given
// counts overlaid. Mirrors the contract /v1/insights provides; we
// can't test the renderer faithfully without it.
func makeHourBuckets(byHour map[int]int) []wire.HourBucket {
	out := make([]wire.HourBucket, 24)
	for i := range out {
		out[i].Hour = i
		out[i].Count = byHour[i]
	}
	return out
}
