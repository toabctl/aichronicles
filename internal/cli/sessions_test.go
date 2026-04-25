package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/redact"
)

// seedStoreForSessions inserts three sessions, each with a user_prompt
// + assistant_message, spread across different times and cwds so
// filter flags have real data to exercise.
func seedStoreForSessions(t *testing.T) *store.Store {
	t.Helper()
	s := testStore(t)

	now := time.Now().UTC()
	fixtures := []struct {
		sessID string
		cwd    string
		when   time.Time
		prompt string
	}{
		{"sess-old", "/work/foo", now.Add(-72 * time.Hour), "first prompt foo"},
		{"sess-mid", "/work/bar", now.Add(-24 * time.Hour), "first prompt bar"},
		{"sess-new", "/work/baz", now.Add(-1 * time.Hour), "first prompt baz"},
	}

	for _, f := range fixtures {
		// user_prompt first, then assistant_message — first_prompt
		// subquery picks the user_prompt.
		envs := []ingest.Envelope{
			{
				V: 1, EventID: uuid.Must(uuid.NewV7()).String(),
				SourceAgent: "claude-code", SourceSessionID: f.sessID,
				Kind: "user_prompt", Role: "user",
				TsSource: f.when, Cwd: f.cwd,
				ContentText: f.prompt,
				Payload:     map[string]any{},
			},
			{
				V: 1, EventID: uuid.Must(uuid.NewV7()).String(),
				SourceAgent: "claude-code", SourceSessionID: f.sessID,
				Kind: "assistant_message", Role: "assistant",
				TsSource: f.when.Add(time.Second), Cwd: f.cwd,
				ContentText: "reply to " + f.prompt,
				Payload:     map[string]any{},
			},
		}
		for _, e := range envs {
			ingest.ApplyRedaction(&e, redact.Default())
			raw, _ := json.Marshal(e)
			tx, err := s.DB().Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if _, err := store.IngestEnvelope(t.Context(), tx, &e, raw, time.Now().UnixMilli()); err != nil {
				_ = tx.Rollback()
				t.Fatalf("ingest: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
	}
	return s
}

func TestBuildSessionsSQL_DefaultLimitIs30(t *testing.T) {
	t.Parallel()
	sqlText, args := buildSessionsSQL(SessionsOptions{})
	if !strings.Contains(sqlText, "FROM sessions s") {
		t.Errorf("SQL should query sessions: %s", sqlText)
	}
	if !strings.Contains(sqlText, "ORDER BY COALESCE(s.ended_at_ms") {
		t.Errorf("SQL should order by ended_at_ms desc: %s", sqlText)
	}
	if len(args) != 1 || args[0] != 30 {
		t.Errorf("args: got %v, want [30]", args)
	}
}

func TestBuildSessionsSQL_AllFilters(t *testing.T) {
	t.Parallel()
	sqlText, args := buildSessionsSQL(SessionsOptions{
		Cwd: "/work", Agent: "claude-code", SinceMs: 100, Limit: 5,
	})
	for _, frag := range []string{`s.cwd = ?`, `s.source_agent = ?`, `s.ended_at_ms >= ?`} {
		if !strings.Contains(sqlText, frag) {
			t.Errorf("missing filter %q: %s", frag, sqlText)
		}
	}
	want := []any{"/work", "claude-code", int64(100), 5}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d]: got %v, want %v", i, args[i], w)
		}
	}
}

func TestRunListSessions_OrdersMostRecentFirst(t *testing.T) {
	t.Parallel()
	s := seedStoreForSessions(t)

	var out bytes.Buffer
	if err := RunListSessions(s, SessionsOptions{}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 sessions, got %d:\n%s", len(lines), out.String())
	}
	// First line should be the newest session (/work/baz).
	if !strings.Contains(lines[0], "/work/baz") {
		t.Errorf("newest session should be first: %s", lines[0])
	}
	if !strings.Contains(lines[2], "/work/foo") {
		t.Errorf("oldest session should be last: %s", lines[2])
	}
}

func TestRunListSessions_IncludesFirstPromptSnippet(t *testing.T) {
	t.Parallel()
	s := seedStoreForSessions(t)
	var out bytes.Buffer
	if err := RunListSessions(s, SessionsOptions{}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "first prompt baz") {
		t.Errorf("expected first-prompt snippet in output:\n%s", out.String())
	}
}

func TestRunListSessions_RespectsCwdFilter(t *testing.T) {
	t.Parallel()
	s := seedStoreForSessions(t)
	var out bytes.Buffer
	if err := RunListSessions(s, SessionsOptions{Cwd: "/work/bar"}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 hit for /work/bar, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "/work/bar") {
		t.Errorf("cwd filter mismatch: %s", lines[0])
	}
}

func TestRunListSessions_RespectsSinceFilter(t *testing.T) {
	t.Parallel()
	s := seedStoreForSessions(t)
	var out bytes.Buffer
	// Only within last 36h: excludes /work/foo (72h old)
	sinceMs := time.Now().Add(-36 * time.Hour).UnixMilli()
	if err := RunListSessions(s, SessionsOptions{SinceMs: sinceMs}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out.String(), "/work/foo") {
		t.Errorf("since should exclude old session:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "/work/bar") {
		t.Errorf("since should include 24h-old session:\n%s", out.String())
	}
}

func TestRunListSessions_RespectsLimit(t *testing.T) {
	t.Parallel()
	s := seedStoreForSessions(t)
	var out bytes.Buffer
	if err := RunListSessions(s, SessionsOptions{Limit: 2}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Errorf("limit=2 should return 2 rows, got %d", len(lines))
	}
}

func TestRunListSessions_EmptyStoreProducesNoOutput(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var out bytes.Buffer
	if err := RunListSessions(s, SessionsOptions{}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("empty store should produce no output, got %q", out.String())
	}
}

func TestFormatSessionRow_ColumnsAndNullHandling(t *testing.T) {
	t.Parallel()
	// Typical row
	got := formatSessionRow(
		"abcdef0123456789",
		sql.NullInt64{Int64: 1700000000000, Valid: true},
		sql.NullInt64{Int64: 1700000060000, Valid: true},
		5,
		sql.NullString{String: "/home/u/proj", Valid: true},
		sql.NullString{String: "hello", Valid: true},
	)
	parts := strings.Split(got, "\t")
	if len(parts) != 6 {
		t.Fatalf("expected 6 tab-separated columns, got %d: %q", len(parts), got)
	}
	if parts[0] != "abcdef01" {
		t.Errorf("session prefix: got %q", parts[0])
	}
	if parts[4] != "/home/u/proj" {
		t.Errorf("cwd: got %q", parts[4])
	}

	// NULL started/ended/cwd/prompt render as "-"
	got = formatSessionRow(
		"zzzzzzzz", sql.NullInt64{}, sql.NullInt64{},
		0, sql.NullString{}, sql.NullString{},
	)
	parts = strings.Split(got, "\t")
	for i := 1; i <= 4; i++ {
		// skip event_count check — that's "0"
		if i == 3 {
			continue
		}
		if parts[i] != "-" {
			t.Errorf("nullable column %d should be '-', got %q", i, parts[i])
		}
	}
}

func TestTruncatePrompt_FlattensAndCaps(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", maxPromptRunes+20) + "\nmore"
	got := truncatePrompt(long)
	runes := []rune(got)
	if len(runes) != maxPromptRunes+1 {
		t.Errorf("expected %d runes, got %d", maxPromptRunes+1, len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("should end with ellipsis: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("newline should be flattened: %q", got)
	}
}
