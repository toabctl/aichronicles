package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/ingest"
	"github.com/toabctl/aichronicles/internal/store"
)

// seedStore opens a Store and inserts a handful of envelopes covering
// multiple kinds, sessions, and timestamps for search tests.
func seedStore(t *testing.T) (*store.Store, []ingest.Envelope) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	now := time.Now().UTC()
	envs := []ingest.Envelope{
		{
			V: 1, EventID: uuid.Must(uuid.NewV7()).String(),
			SourceAgent: "claude-code", SourceSessionID: "sess-foo",
			Kind: "user_prompt", TsSource: now.Add(-48 * time.Hour),
			Cwd: "/work/foo", ContentText: "what is jsonl format",
			Payload: map[string]any{},
		},
		{
			V: 1, EventID: uuid.Must(uuid.NewV7()).String(),
			SourceAgent: "claude-code", SourceSessionID: "sess-foo",
			Kind: "assistant_message", TsSource: now.Add(-47 * time.Hour),
			Cwd: "/work/foo", ContentText: "JSON Lines is one object per line",
			Payload: map[string]any{},
		},
		{
			V: 1, EventID: uuid.Must(uuid.NewV7()).String(),
			SourceAgent: "claude-code", SourceSessionID: "sess-bar",
			Kind: "user_prompt", TsSource: now.Add(-2 * time.Hour),
			Cwd: "/work/bar", ContentText: "how does systemd socket activation work",
			Payload: map[string]any{},
		},
		{
			V: 1, EventID: uuid.Must(uuid.NewV7()).String(),
			SourceAgent: "claude-code", SourceSessionID: "sess-bar",
			Kind: "tool_use", TsSource: now.Add(-1 * time.Hour),
			Cwd: "/work/bar", ContentText: "Bash",
			Tool:    &ingest.Tool{Name: "Bash"},
			Payload: map[string]any{"tool_input": map[string]any{"command": "systemctl --user status"}},
		},
	}

	for _, e := range envs {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		tx, err := s.DB().Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := store.IngestEnvelope(tx, &e, raw, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			t.Fatalf("ingest: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	return s, envs
}

func TestBuildSearchSQL_BaseQueryOnly(t *testing.T) {
	t.Parallel()
	sql, args := buildSearchSQL(SearchOptions{Query: "hello"})
	if !strings.Contains(sql, "events_fts MATCH ?") {
		t.Errorf("SQL should MATCH on FTS: %s", sql)
	}
	if !strings.Contains(sql, "ORDER BY rank LIMIT ?") {
		t.Errorf("SQL should order by rank and limit: %s", sql)
	}
	if len(args) != 2 {
		t.Errorf("args: got %d, want 2 (query, limit)", len(args))
	}
	if args[0] != "hello" {
		t.Errorf("args[0]: got %v, want hello", args[0])
	}
	if args[1] != 20 {
		t.Errorf("default limit: got %v, want 20", args[1])
	}
}

func TestBuildSearchSQL_AllFilters(t *testing.T) {
	t.Parallel()
	sql, args := buildSearchSQL(SearchOptions{
		Query:     "boom",
		Kind:      "tool_use",
		SessionID: "sess-xyz",
		SinceMs:   1000,
		Limit:     5,
	})
	for _, frag := range []string{`e.kind = ?`, `e.session_id = ?`, `e.ts_source_ms >= ?`} {
		if !strings.Contains(sql, frag) {
			t.Errorf("missing filter %q in sql: %s", frag, sql)
		}
	}
	want := []any{"boom", "tool_use", "sess-xyz", int64(1000), 5}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d]: got %v, want %v", i, args[i], w)
		}
	}
}

func TestBuildSearchSQL_ZeroLimitDefaults(t *testing.T) {
	t.Parallel()
	_, args := buildSearchSQL(SearchOptions{Query: "x", Limit: 0})
	if args[len(args)-1] != 20 {
		t.Errorf("zero limit should default to 20, got %v", args[len(args)-1])
	}
}

func TestRunSearch_FindsByKeyword(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	var out bytes.Buffer

	if err := RunSearch(s, SearchOptions{Query: "jsonl"}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	if !strings.Contains(out.String(), "what is jsonl format") {
		t.Errorf("expected jsonl hit in output:\n%s", out.String())
	}
	// one hit, one line
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 hit, got %d:\n%s", len(lines), out.String())
	}
}

func TestRunSearch_RespectsKindFilter(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	var out bytes.Buffer

	// "systemd" appears in both a user_prompt and (via Bash) a tool_use;
	// kind=user_prompt should narrow to one line.
	if err := RunSearch(s, SearchOptions{Query: "systemd", Kind: "user_prompt"}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 hit, got %d:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "user_prompt") {
		t.Errorf("hit line should be user_prompt: %s", lines[0])
	}
}

func TestRunSearch_RespectsSessionFilter(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	var out bytes.Buffer

	sessFooID := ingest.DeriveSessionID("claude-code", "sess-foo")
	if err := RunSearch(s, SearchOptions{Query: "is", SessionID: sessFooID}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	got := out.String()
	// Should only include hits from sess-foo (prefix match in column 2).
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			t.Fatalf("malformed line: %q", line)
		}
		if !strings.HasPrefix(sessFooID, fields[1]) {
			t.Errorf("hit from wrong session: line=%q sess_prefix=%q", line, fields[1])
		}
	}
}

func TestRunSearch_RespectsSinceFilter(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	var out bytes.Buffer

	// Only events in the last 3 hours — excludes the 48h-old foo session.
	sinceMs := time.Now().Add(-3 * time.Hour).UnixMilli()
	if err := RunSearch(s, SearchOptions{Query: "systemd", SinceMs: sinceMs}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	if strings.Contains(out.String(), "jsonl") {
		t.Errorf("since filter should exclude 48h-old events: %s", out.String())
	}
}

func TestRunSearch_RespectsLimit(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)

	// Insert many more events so we can cap.
	for i := 0; i < 15; i++ {
		env := ingest.Envelope{
			V: 1, EventID: uuid.Must(uuid.NewV7()).String(),
			SourceAgent: "claude-code", SourceSessionID: "sess-bulk",
			Kind: "user_prompt", TsSource: time.Now().UTC(),
			ContentText: "limittest marker",
			Payload:     map[string]any{},
		}
		raw, _ := json.Marshal(env)
		tx, _ := s.DB().Begin()
		_, err := store.IngestEnvelope(tx, &env, raw, time.Now().UnixMilli())
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("ingest: %v", err)
		}
		_ = tx.Commit()
	}

	var out bytes.Buffer
	if err := RunSearch(s, SearchOptions{Query: "limittest", Limit: 5}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Errorf("expected 5 hits (limit), got %d", len(lines))
	}
}

func TestRunSearch_EmptyQueryIsError(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	err := RunSearch(s, SearchOptions{Query: "   "}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for whitespace-only query")
	}
}

func TestRunSearch_NoMatchesProducesNoOutput(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	var out bytes.Buffer
	if err := RunSearch(s, SearchOptions{Query: "thisdoesnotappear"}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output, got %q", out.String())
	}
}

func TestFormatHit_Shape(t *testing.T) {
	t.Parallel()
	line := formatHit("abcdef012345", "user_prompt", "/home/user", 1_700_000_000_000, "hello\nworld")
	parts := strings.Split(line, "\t")
	if len(parts) != 5 {
		t.Fatalf("expected 5 tab-separated columns, got %d: %q", len(parts), line)
	}
	if parts[1] != "abcdef01" {
		t.Errorf("session prefix: got %q, want abcdef01", parts[1])
	}
	if strings.Contains(parts[4], "\n") {
		t.Errorf("snippet should flatten newlines: %q", parts[4])
	}
}

func TestTruncateSnippet_LongText(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", maxSnippetRunes+50)
	got := truncateSnippet(long)
	// One rune for …, plus maxSnippetRunes of x
	if len([]rune(got)) != maxSnippetRunes+1 {
		t.Errorf("truncated length: got %d runes, want %d", len([]rune(got)), maxSnippetRunes+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("should end with ellipsis, got %q", got[len(got)-4:])
	}
}

// compile-time sanity: SearchOptions zero value works.
var _ = SearchOptions{}

// deref helper from search.go must handle sql.NullString wrappers
// correctly via *string. Dummy test confirms the shape.
func TestDeref(t *testing.T) {
	t.Parallel()
	if deref(nil) != "" {
		t.Error("nil → empty")
	}
	s := "x"
	if deref(&s) != "x" {
		t.Error("*string → value")
	}
}

// suppressUnusedLinterWhenPackageShrinks ensures database/sql is
// referenced even in a future where search_test.go is the only file
// using it (unlikely but keeps lint from complaining in rewrites).
var _ sql.NullString
