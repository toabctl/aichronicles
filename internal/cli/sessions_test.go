package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
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
		envs := []events.Envelope{
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
			events.ApplyRedaction(&e, redact.Default())
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

func TestRunListSessions_OrdersMostRecentFirst(t *testing.T) {
	t.Parallel()
	s := seedStoreForSessions(t)
	c := apiForStore(t, s)

	var out bytes.Buffer
	if err := RunListSessions(t.Context(), c, SessionsOptions{}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// Header + 3 data rows.
	if len(lines) != 4 {
		t.Fatalf("expected header + 3 sessions = 4 lines, got %d:\n%s", len(lines), out.String())
	}
	// Header is lines[0]; lines[1] is newest, lines[3] is oldest.
	if !strings.Contains(lines[1], "/work/baz") {
		t.Errorf("newest session should be first data row: %s", lines[1])
	}
	if !strings.Contains(lines[3], "/work/foo") {
		t.Errorf("oldest session should be last: %s", lines[3])
	}
}

func TestRunListSessions_IncludesFirstPromptSnippet(t *testing.T) {
	t.Parallel()
	s := seedStoreForSessions(t)
	c := apiForStore(t, s)
	var out bytes.Buffer
	if err := RunListSessions(t.Context(), c, SessionsOptions{}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "first prompt baz") {
		t.Errorf("expected first-prompt snippet in output:\n%s", out.String())
	}
}

func TestRunListSessions_RespectsCwdFilter(t *testing.T) {
	t.Parallel()
	s := seedStoreForSessions(t)
	c := apiForStore(t, s)
	var out bytes.Buffer
	if err := RunListSessions(t.Context(), c, SessionsOptions{Cwd: "/work/bar"}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// Header + 1 data row.
	if len(lines) != 2 {
		t.Fatalf("expected header + 1 hit for /work/bar = 2 lines, got %d", len(lines))
	}
	if !strings.Contains(lines[1], "/work/bar") {
		t.Errorf("cwd filter mismatch: %s", lines[1])
	}
}

func TestRunListSessions_RespectsSinceFilter(t *testing.T) {
	t.Parallel()
	s := seedStoreForSessions(t)
	c := apiForStore(t, s)
	var out bytes.Buffer
	// Only within last 36h: excludes /work/foo (72h old)
	sinceMs := time.Now().Add(-36 * time.Hour).UnixMilli()
	if err := RunListSessions(t.Context(), c, SessionsOptions{SinceMs: sinceMs}, &out); err != nil {
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
	c := apiForStore(t, s)
	var out bytes.Buffer
	if err := RunListSessions(t.Context(), c, SessionsOptions{Limit: 2}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	// One header line + N data rows.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Errorf("limit=2 should return header + 2 rows = 3 lines, got %d", len(lines))
	}
}

func TestRunListSessions_EmptyStoreShowsEmptyStateLine(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	c := apiForStore(t, s)
	var out bytes.Buffer
	if err := RunListSessions(t.Context(), c, SessionsOptions{}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "(no sessions matched)") {
		t.Errorf("expected empty-state line, got %q", out.String())
	}
}

func TestRunListSessions_JSONFormatIsArray(t *testing.T) {
	t.Parallel()
	s := seedStoreForSessions(t)
	c := apiForStore(t, s)
	var out bytes.Buffer
	if err := RunListSessions(t.Context(), c, SessionsOptions{Format: FormatJSON}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	body := out.String()
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Fatalf("expected JSON array, got %q", body)
	}
	// Three seeded sessions → three session_id keys in the payload.
	if got := strings.Count(body, "\"session_id\""); got != 3 {
		t.Errorf("session_id count: got %d, want 3:\n%s", got, body)
	}
}

func TestRunListSessions_JSONEmptyStoreReturnsArray(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	c := apiForStore(t, s)
	var out bytes.Buffer
	if err := RunListSessions(t.Context(), c, SessionsOptions{Format: FormatJSON}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Must be a valid (empty) array, not the "(no sessions matched)" line.
	got := strings.TrimSpace(out.String())
	if got != "[]" {
		t.Errorf("empty JSON output should be [], got %q", got)
	}
}

func TestFormatSessionRow_ColumnsAndNullHandling(t *testing.T) {
	t.Parallel()
	started := int64(1700000000000)
	ended := int64(1700000060000)
	cwd := "/home/u/proj"
	prompt := "hello"
	got := formatSessionRow(wire.SessionDigest{
		ID:          "abcdef0123456789",
		StartedAtMs: &started,
		EndedAtMs:   &ended,
		EventCount:  5,
		Cwd:         &cwd,
		FirstPrompt: &prompt,
	})
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

	// nil started/ended/cwd/prompt render as "-"
	got = formatSessionRow(wire.SessionDigest{ID: "zzzzzzzz"})
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
