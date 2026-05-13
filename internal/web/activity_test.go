package web

import (
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

func TestSessionStatus(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	mk := func(kind string, ageBack time.Duration) *store.LiveEvent {
		return &store.LiveEvent{
			Kind:       kind,
			TsSourceMs: now.Add(-ageBack).UnixMilli(),
		}
	}

	cases := []struct {
		name        string
		latest      *store.LiveEvent
		wantStatus  string
		wantInTitle string
	}{
		{
			name:        "session_end kind flips to ended even if recent",
			latest:      mk("session_end", 30*time.Second),
			wantStatus:  "ended",
			wantInTitle: "ended",
		},
		{
			name:        "session_end kind on stale event still ends",
			latest:      mk("session_end", 7*24*time.Hour),
			wantStatus:  "ended",
			wantInTitle: "ended",
		},
		{
			name:        "active when latest event within window",
			latest:      mk("user_prompt", 1*time.Minute),
			wantStatus:  "active",
			wantInTitle: "active",
		},
		{
			name:        "active right at window boundary minus epsilon",
			latest:      mk("user_prompt", activityWindow-time.Second),
			wantStatus:  "active",
			wantInTitle: "active",
		},
		{
			name:        "idle once outside the window",
			latest:      mk("user_prompt", activityWindow+time.Second),
			wantStatus:  "idle",
			wantInTitle: "idle",
		},
		{
			name:        "idle for very old session",
			latest:      mk("tool_use", 30*24*time.Hour),
			wantStatus:  "idle",
			wantInTitle: "idle",
		},
		{
			name:        "nil latest → idle, no events yet",
			latest:      nil,
			wantStatus:  "idle",
			wantInTitle: "no events yet",
		},
		{
			name:        "zero ts on latest → idle, no events yet",
			latest:      &store.LiveEvent{Kind: "user_prompt", TsSourceMs: 0},
			wantStatus:  "idle",
			wantInTitle: "no events yet",
		},
		{
			name:        "future timestamp falls back to idle (defensive)",
			latest:      mk("user_prompt", -1*time.Hour),
			wantStatus:  "idle",
			wantInTitle: "idle",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotStatus, gotTitle := sessionStatus(tc.latest, now)
			if gotStatus != tc.wantStatus {
				t.Errorf("status: got %q, want %q", gotStatus, tc.wantStatus)
			}
			if !strings.Contains(gotTitle, tc.wantInTitle) {
				t.Errorf("title %q does not contain %q", gotTitle, tc.wantInTitle)
			}
		})
	}
}

func TestRenderStatusDot_InitialAndOOB(t *testing.T) {
	t.Parallel()
	const id = "00000000-0000-0000-0000-000000000abc"
	plain := renderStatusDot(id, "active", "active just now", false)
	oob := renderStatusDot(id, "active", "active just now", true)

	for _, want := range []string{
		`id="status-00000000-0000-0000-0000-000000000abc"`,
		`class="status status-active"`,
		`title="active just now"`,
		`>●</span>`,
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("plain dot missing %q\nfull: %s", want, plain)
		}
		if !strings.Contains(oob, want) {
			t.Errorf("oob dot missing %q\nfull: %s", want, oob)
		}
	}

	if strings.Contains(plain, "hx-swap-oob") {
		t.Errorf("plain dot must NOT carry hx-swap-oob:\n%s", plain)
	}
	if !strings.Contains(oob, `hx-swap-oob="true"`) {
		t.Errorf("oob dot must carry hx-swap-oob=true:\n%s", oob)
	}
}

func TestRenderStatusDot_EscapesHostileInput(t *testing.T) {
	t.Parallel()
	// Defense in depth: status / title come from sessionStatus, which
	// only emits known values today, but the renderer should not let
	// any hostile fragment escape the attribute.
	got := renderStatusDot("\"><script>", "active", `"><b>boom</b>`, false)
	if strings.Contains(got, "<script>") {
		t.Errorf("session_id leaked unescaped:\n%s", got)
	}
	if strings.Contains(got, "<b>boom</b>") {
		t.Errorf("title leaked unescaped:\n%s", got)
	}
}

func TestRenderLatestEventCell(t *testing.T) {
	t.Parallel()
	e := store.LiveEvent{
		EventID:    "evt-1",
		SessionID:  "sess-1",
		Kind:       "user_prompt",
		TsSourceMs: time.Date(2026, 4, 24, 15, 42, 1, 0, time.UTC).UnixMilli(),
		Snippet:    ptrTo("how do I parse jsonl in Go"),
	}
	got := renderLatestEventCell(e)
	for _, want := range []string{
		`<span class="ts">15:42:01</span>`,
		`<span class="badge badge-user_prompt">user_prompt</span>`,
		`<span class="snippet">how do I parse jsonl in Go</span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\nfull: %s", want, got)
		}
	}
}

func TestRenderLatestEventCell_EscapesSnippetAndKind(t *testing.T) {
	t.Parallel()
	e := store.LiveEvent{
		Kind:       `<img src=x onerror=1>`,
		TsSourceMs: 1,
		Snippet:    ptrTo(`</span><script>alert(1)</script>`),
	}
	got := renderLatestEventCell(e)
	if strings.Contains(got, "<script>") {
		t.Errorf("snippet leaked unescaped:\n%s", got)
	}
	if strings.Contains(got, `<img src=x`) {
		t.Errorf("kind leaked unescaped:\n%s", got)
	}
}

func TestRenderLatestEventCell_FlattenSnippetWhitespace(t *testing.T) {
	t.Parallel()
	e := store.LiveEvent{
		Kind:       "tool_use",
		TsSourceMs: 1,
		Snippet:    ptrTo("line one\nline two\tline three"),
	}
	got := renderLatestEventCell(e)
	// truncateForStream replaces \n and \t with spaces so the SSE
	// data field stays on one line.
	if strings.Contains(got, "\n") || strings.Contains(got, "\t") {
		t.Errorf("expected whitespace flattened to single line:\n%q", got)
	}
}
