package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/events"
)

// seedStore opens a Store and inserts a handful of envelopes covering
// multiple kinds, sessions, and timestamps for search tests.
func seedStore(t *testing.T) (*store.Store, []events.Envelope) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	now := time.Now().UTC()
	envs := []events.Envelope{
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
			Tool:    &events.Tool{Name: "Bash"},
			Payload: map[string]any{"tool_input": map[string]any{"command": "systemctl --user status"}},
		},
	}

	for _, e := range envs {
		events.ApplyRedaction(&e, redact.Default())
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
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
	return s, envs
}

// SQL-composition assertions for SearchOptions used to live here as
// TestBuildSearchSQL_*; the equivalent behavior tests now live in
// internal/store/search_test.go and exercise the real query end to
// end against a seeded SQLite. Deleting the string-shape assertions
// removes a brittle layer that broke on every harmless query
// rewrite.

func TestRunSearch_FindsByKeyword(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	var out bytes.Buffer

	if err := RunSearch(t.Context(), apiForStore(t, s), SearchOptions{Query: "jsonl"}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	if !strings.Contains(out.String(), "what is jsonl format") {
		t.Errorf("expected jsonl hit in output:\n%s", out.String())
	}
	// header + 1 data row.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Errorf("expected header + 1 hit = 2 lines, got %d:\n%s", len(lines), out.String())
	}
}

func TestRunSearch_RespectsKindFilter(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	var out bytes.Buffer

	// "systemd" appears in both a user_prompt and (via Bash) a tool_use;
	// kind=user_prompt should narrow to one line.
	if err := RunSearch(t.Context(), apiForStore(t, s), SearchOptions{Query: "systemd", Kind: "user_prompt"}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// header + 1 data row.
	if len(lines) != 2 {
		t.Fatalf("expected header + 1 hit = 2 lines, got %d:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[1], "user_prompt") {
		t.Errorf("hit line should be user_prompt: %s", lines[1])
	}
}

func TestRunSearch_RespectsSessionFilter(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	var out bytes.Buffer

	sessFooID := events.DeriveSessionID("claude-code", "sess-foo")
	if err := RunSearch(t.Context(), apiForStore(t, s), SearchOptions{Query: "is", SessionID: sessFooID}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	got := out.String()
	// Should only include hits from sess-foo. Skip the header line and
	// the empty-state line; the timestamp column now contains its own
	// spaces (relative form), so look for the session-prefix substring
	// rather than parsing column-by-column.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	prefix := sessFooID[:8]
	for i, line := range lines {
		if i == 0 || line == "" {
			continue
		}
		if !strings.Contains(line, prefix) {
			t.Errorf("hit from wrong session: line=%q want session prefix %q", line, prefix)
		}
	}
}

func TestRunSearch_RespectsSinceFilter(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	var out bytes.Buffer

	// Only events in the last 3 hours — excludes the 48h-old foo session.
	sinceMs := time.Now().Add(-3 * time.Hour).UnixMilli()
	if err := RunSearch(t.Context(), apiForStore(t, s), SearchOptions{Query: "systemd", SinceMs: sinceMs}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	if strings.Contains(out.String(), "jsonl") {
		t.Errorf("since filter should exclude 48h-old events: %s", out.String())
	}
}

func TestRunSearch_RespectsLimit(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)

	// Insert many more events so we can cap. Content must be distinct
	// per iteration so the default dedup doesn't collapse them into
	// one partition — this test is about --limit, not about dedupe.
	for i := 0; i < 15; i++ {
		env := events.Envelope{
			V: 1, EventID: uuid.Must(uuid.NewV7()).String(),
			SourceAgent: "claude-code", SourceSessionID: "sess-bulk",
			Kind: "user_prompt", TsSource: time.Now().UTC(),
			ContentText: fmt.Sprintf("limittest marker %d", i),
			Payload:     map[string]any{},
		}
		events.ApplyRedaction(&env, redact.Default())
		raw, _ := json.Marshal(env)
		tx, _ := s.DB().Begin()
		_, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli())
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("ingest: %v", err)
		}
		_ = tx.Commit()
	}

	var out bytes.Buffer
	if err := RunSearch(t.Context(), apiForStore(t, s), SearchOptions{Query: "limittest", Limit: 5}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// header + 5 data rows.
	if len(lines) != 6 {
		t.Errorf("expected header + 5 hits = 6 lines, got %d", len(lines))
	}
}

func TestRunSearch_EmptyQueryIsError(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	err := RunSearch(t.Context(), apiForStore(t, s), SearchOptions{Query: "   "}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for whitespace-only query")
	}
}

func TestRunSearch_NoMatchesShowsEmptyStateLine(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	var out bytes.Buffer
	if err := RunSearch(t.Context(), apiForStore(t, s), SearchOptions{Query: "thisdoesnotappear"}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	if !strings.Contains(out.String(), "(no hits") {
		t.Errorf("expected empty-state line, got %q", out.String())
	}
}

// TestRunSearch_PrefixMatchFromBareToken proves the user no longer
// needs to know FTS5 syntax: typing "json" matches "jsonl" because
// the parser appends *.
func TestRunSearch_PrefixMatchFromBareToken(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	var out bytes.Buffer
	if err := RunSearch(t.Context(), apiForStore(t, s), SearchOptions{Query: "json"}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	if !strings.Contains(out.String(), "jsonl") {
		t.Errorf("prefix match failed: %s", out.String())
	}
}

// TestRunSearch_UnclosedQuoteIsParseError verifies the parser's
// ErrSyntax surfaces as a wrapped error rather than an opaque
// SQLite "fts5: syntax error" string.
func TestRunSearch_UnclosedQuoteIsParseError(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	err := RunSearch(t.Context(), apiForStore(t, s), SearchOptions{Query: `find "this without close`}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for unclosed quote")
	}
	// The api parses the query server-side and rejects unclosed
	// quotes with a 400 + "Invalid q" problem; the apiclient
	// surfaces that as a wrapped HTTPError. We just want any
	// signal that the parse failure was reported, not papered over.
	if !strings.Contains(err.Error(), "Invalid q") {
		t.Errorf("expected api invalid-q error, got: %v", err)
	}
}

// seedDuplicateTurn inserts two envelopes for the same logical turn:
// one with transport="hook" and one with transport="import". Same
// source_session_id so they collapse into the same derived session_id.
// Same role, kind, and content. Different event_ids, different
// timestamps (hook observed earlier, transcript a bit later), so the
// dedup logic has to use content equality rather than timestamp match.
func seedDuplicateTurn(t *testing.T, s *store.Store) (sessionID, hookEventID, importEventID string) {
	t.Helper()
	hookEnv := events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-dup",
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC),
		Cwd:             "/work/dup",
		ContentText:     "duplicated turn text marker",
		Payload:         map[string]any{"from": "hook"},
		Transport:       "hook",
	}
	importEnv := hookEnv
	importEnv.EventID = uuid.Must(uuid.NewV7()).String()
	importEnv.TsSource = hookEnv.TsSource.Add(50 * time.Millisecond)
	importEnv.Payload = map[string]any{"from": "import"}
	importEnv.Transport = "import"

	for _, e := range []events.Envelope{hookEnv, importEnv} {
		events.ApplyRedaction(&e, redact.Default())
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
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
	return events.DeriveSessionID("claude-code", "sess-dup"), hookEnv.EventID, importEnv.EventID
}

func TestRunSearch_DedupeCollapsesDuplicateTurn(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	seedDuplicateTurn(t, s)

	var out bytes.Buffer
	if err := RunSearch(t.Context(), apiForStore(t, s), SearchOptions{Query: "duplicated"}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// header + 1 deduped row.
	if len(lines) != 2 {
		t.Fatalf("expected header + 1 deduped hit = 2 lines, got %d:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[1], "duplicated turn text marker") {
		t.Errorf("unexpected hit: %s", lines[1])
	}
}

func TestRunSearch_NoDedupSurfacesBothCopies(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	seedDuplicateTurn(t, s)

	var out bytes.Buffer
	if err := RunSearch(t.Context(), apiForStore(t, s), SearchOptions{Query: "duplicated", NoDedup: true}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// header + 2 (un-deduped) rows.
	if len(lines) != 3 {
		t.Fatalf("--no-dedup expected header + 2 hits = 3 lines, got %d:\n%s", len(lines), out.String())
	}
}

func TestRunSearch_DedupePrefersHookTransport(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	sessionID, hookID, importID := seedDuplicateTurn(t, s)

	// Drive the real SearchEvents path the CLI uses. The deduped
	// row should be the hook event, whose ts_source_ms ends in :000;
	// the import was +50ms.
	hits, err := store.SearchEvents(t.Context(), s.DB(), store.SearchEventOpts{
		Query: "duplicated", Limit: 1,
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("dedup: got %d hits, want 1", len(hits))
	}
	if hits[0].TsSourceMs%1000 != 0 {
		t.Errorf("dedupe picked the import row (ts_ms=%d); hook was expected", hits[0].TsSourceMs)
	}
	if hits[0].SessionID != sessionID {
		t.Errorf("session_id: got %q, want %q", hits[0].SessionID, sessionID)
	}

	// Sanity: both events exist in the raw table (we haven't deleted
	// anything — dedupe is query-time only).
	var nRaw int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM raw_envelopes WHERE event_id IN (?, ?)`,
		hookID, importID).Scan(&nRaw)
	if nRaw != 2 {
		t.Errorf("raw_envelopes should retain both rows, got %d", nRaw)
	}
}

func TestRunSearch_DedupeDoesNotCollapseDistinctContent(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)

	// Two events in same session, same role, same kind — but different
	// content. Must NOT be deduped.
	base := events.Envelope{
		V:               1,
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-distinct",
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC),
		Payload:         map[string]any{},
		Transport:       "hook",
	}
	for _, txt := range []string{"distinct one marker", "distinct two marker"} {
		env := base
		env.EventID = uuid.Must(uuid.NewV7()).String()
		env.ContentText = txt
		events.ApplyRedaction(&env, redact.Default())
		raw, _ := json.Marshal(env)
		tx, _ := s.DB().Begin()
		_, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli())
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("ingest: %v", err)
		}
		_ = tx.Commit()
	}

	var out bytes.Buffer
	if err := RunSearch(t.Context(), apiForStore(t, s), SearchOptions{Query: "distinct"}, &out); err != nil {
		t.Fatalf("RunSearch: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// header + 2 distinct-content rows.
	if len(lines) != 3 {
		t.Errorf("distinct-content rows should both surface (header + 2 = 3 lines), got %d:\n%s", len(lines), out.String())
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
