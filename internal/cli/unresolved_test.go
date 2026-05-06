package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/api"
	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/store"
	apiwire "github.com/toabctl/aichronicles/internal/wire"
)

// plantSummaryWithUnresolved seeds the store with a session +
// summary llm_output whose body contains the named unresolved
// items. Goes through the test store directly; the api reads
// the same DB.
func plantSummaryWithUnresolved(t *testing.T, s *store.Store, sessID, cwd, topic string, endedAgo time.Duration, items []string) {
	t.Helper()
	now := time.Now().UTC()
	endedAt := now.Add(-endedAgo).UnixMilli()
	startedAt := endedAt - 60*60*1000

	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms, cwd)
		 VALUES (?, 'claude-code', ?, ?, ?, ?)`,
		sessID, "src-"+sessID, startedAt, endedAt, cwd,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	var b strings.Builder
	b.WriteString(`{"topic":"` + topic + `","what_was_done":["x"],"unresolved":[`)
	for i, u := range items {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + u + `"`)
	}
	b.WriteString(`],"key_files":[],"links":[]}`)
	if _, err := s.DB().Exec(
		`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms)
		 VALUES (?, 'summary', ?, 'h-'||?, 'fake-model', ?)`,
		sessID, b.String(), sessID, endedAt,
	); err != nil {
		t.Fatalf("seed summary: %v", err)
	}
}

func TestRenderUnresolved_TextFormFitForHook(t *testing.T) {
	t.Parallel()
	now := time.Now()
	items := []apiwire.UnresolvedItem{
		{
			SessionID:    "11111111-1111-1111-1111-111111111111",
			SessionShort: "11111111",
			EndedAtMs:    now.Add(-2 * time.Hour).UnixMilli(),
			Topic:        "auth middleware refactor",
			Item:         "document the new fallback flow",
		},
		{
			SessionID:    "22222222-2222-2222-2222-222222222222",
			SessionShort: "22222222",
			EndedAtMs:    now.Add(-3 * 24 * time.Hour).UnixMilli(),
			Topic:        "ingest pipeline",
			Item:         "add the redaction passthrough test",
		},
	}
	var out bytes.Buffer
	if err := renderUnresolved(&out, "/repo/x", items, FormatTable); err != nil {
		t.Fatalf("renderUnresolved: %v", err)
	}
	body := out.String()

	for _, want := range []string{
		"aichronicles:",
		"2 unresolved item(s)",
		"/repo/x",
		"11111111",
		"auth middleware refactor",
		"document the new fallback flow",
		"22222222",
		"ingest pipeline",
		"add the redaction passthrough test",
		"2h ago",
		"3d ago",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
}

func TestRenderUnresolved_EmptyHasFriendlyMessage(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := renderUnresolved(&out, "/repo/empty", nil, FormatTable); err != nil {
		t.Fatalf("renderUnresolved: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "no unresolved items") {
		t.Errorf("expected friendly empty message:\n%s", body)
	}
	if strings.Count(body, "\n") != 1 {
		t.Errorf("expected exactly 1 line of output, got:\n%s", body)
	}
}

func TestRenderUnresolved_JSONShape(t *testing.T) {
	t.Parallel()
	items := []apiwire.UnresolvedItem{
		{SessionID: "abc", SessionShort: "abc", EndedAtMs: 1, Topic: "t", Item: "i"},
	}
	var out bytes.Buffer
	if err := renderUnresolved(&out, "/repo/y", items, FormatJSON); err != nil {
		t.Fatalf("renderUnresolved JSON: %v", err)
	}
	body := out.String()
	for _, want := range []string{
		`"cwd": "/repo/y"`,
		`"session_id": "abc"`,
		`"topic": "t"`,
		`"item": "i"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("JSON missing %q:\n%s", want, body)
		}
	}
}

func TestRelativeTimeOrAbsent_Branches(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		ts   time.Time
		want string
	}{
		{"zero", time.UnixMilli(0), "still active"},
		{"future", now.Add(time.Hour), "future?"},
		{"30s", now.Add(-30 * time.Second), "just now"},
		{"5m", now.Add(-5 * time.Minute), "5m ago"},
		{"3h", now.Add(-3 * time.Hour), "3h ago"},
		{"5d", now.Add(-5 * 24 * time.Hour), "5d ago"},
		{"60d", now.Add(-60 * 24 * time.Hour), "2026-02-27"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := tc.ts.UnixMilli()
			if tc.name == "zero" {
				ms = 0
			}
			got := relativeTimeOrAbsent(ms, now)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUnresolvedEndToEnd seeds the store, stands up the real
// internal/api handlers under httptest, runs the migrated
// command body through an apiclient pointed at it, and asserts
// the rendered output contains the expected items. Replaces the
// pre-migration test that called the store loader directly.
func TestUnresolvedEndToEnd(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	const cwd = "/work/e2e"
	plantSummaryWithUnresolved(t, s,
		"00000000-0000-0000-0000-0000000000aa", cwd,
		"morning session", 2*time.Hour,
		[]string{"land the migration", "open a follow-up issue"})

	srv := httptest.NewServer(api.NewServer(s, nil).Handler())
	t.Cleanup(srv.Close)
	c := apiclient.NewClientForTesting(srv.Client(), srv.URL)

	var out bytes.Buffer
	err := runUnresolved(context.Background(), c, runUnresolvedOpts{
		Cwd:         cwd,
		Since:       defaultUnresolvedSince,
		MaxSessions: 5,
		MaxItems:    5,
		Format:      FormatTable,
		Out:         &out,
	})
	if err != nil {
		t.Fatalf("runUnresolved: %v", err)
	}
	for _, want := range []string{
		"land the migration",
		"open a follow-up issue",
		"morning session",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("e2e output missing %q:\n%s", want, out.String())
		}
	}
}
