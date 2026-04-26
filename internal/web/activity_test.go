package web

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

func TestSessionStatus(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		endedMs     sql.NullInt64
		latestTsMs  int64
		wantStatus  string
		wantInTitle string
	}{
		{
			name:        "ended takes priority over recency",
			endedMs:     sql.NullInt64{Int64: now.Add(-time.Hour).UnixMilli(), Valid: true},
			latestTsMs:  now.Add(-30 * time.Second).UnixMilli(),
			wantStatus:  "ended",
			wantInTitle: "ended",
		},
		{
			name:        "ended_at_ms zero treated as not-ended",
			endedMs:     sql.NullInt64{Int64: 0, Valid: true},
			latestTsMs:  now.Add(-1 * time.Minute).UnixMilli(),
			wantStatus:  "active",
			wantInTitle: "active",
		},
		{
			name:        "active when latest event within window",
			endedMs:     sql.NullInt64{},
			latestTsMs:  now.Add(-1 * time.Minute).UnixMilli(),
			wantStatus:  "active",
			wantInTitle: "active",
		},
		{
			name:        "active right at window boundary minus epsilon",
			endedMs:     sql.NullInt64{},
			latestTsMs:  now.Add(-(activityWindow - time.Second)).UnixMilli(),
			wantStatus:  "active",
			wantInTitle: "active",
		},
		{
			name:        "idle once outside the window",
			endedMs:     sql.NullInt64{},
			latestTsMs:  now.Add(-(activityWindow + time.Second)).UnixMilli(),
			wantStatus:  "idle",
			wantInTitle: "idle",
		},
		{
			name:        "idle for very old session",
			endedMs:     sql.NullInt64{},
			latestTsMs:  now.Add(-30 * 24 * time.Hour).UnixMilli(),
			wantStatus:  "idle",
			wantInTitle: "idle",
		},
		{
			name:        "no events yet → idle",
			endedMs:     sql.NullInt64{},
			latestTsMs:  0,
			wantStatus:  "idle",
			wantInTitle: "no events yet",
		},
		{
			name:        "future timestamp falls back to idle (defensive)",
			endedMs:     sql.NullInt64{},
			latestTsMs:  now.Add(time.Hour).UnixMilli(),
			wantStatus:  "idle",
			wantInTitle: "idle",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotStatus, gotTitle := sessionStatus(tc.endedMs, tc.latestTsMs, now)
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
		Snippet:    sql.NullString{String: "how do I parse jsonl in Go", Valid: true},
	}
	got := renderLatestEventCell(e)
	for _, want := range []string{
		`<span class="ts">15:42:01</span>`,
		`<span class="badge">user_prompt</span>`,
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
		Snippet:    sql.NullString{String: `</span><script>alert(1)</script>`, Valid: true},
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
		Snippet:    sql.NullString{String: "line one\nline two\tline three", Valid: true},
	}
	got := renderLatestEventCell(e)
	// truncateForStream replaces \n and \t with spaces so the SSE
	// data field stays on one line.
	if strings.Contains(got, "\n") || strings.Contains(got, "\t") {
		t.Errorf("expected whitespace flattened to single line:\n%q", got)
	}
}

func TestStatusForLiveEvent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		kind       string
		wantStatus string
	}{
		{"normal event makes session active", "user_prompt", "active"},
		{"tool_use also active", "tool_use", "active"},
		{"session_end flips to ended", "session_end", "ended"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := store.LiveEvent{Kind: tc.kind, TsSourceMs: now.UnixMilli()}
			gotStatus, _ := statusForLiveEvent(e, now)
			if gotStatus != tc.wantStatus {
				t.Errorf("status: got %q, want %q", gotStatus, tc.wantStatus)
			}
		})
	}
}
