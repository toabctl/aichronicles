package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

// ingestText is a shorthand that drops one envelope into the store
// with the given content and source_session_id, walking the same
// IngestEnvelope path production traffic uses (so the events_fts
// triggers fire). Returns the derived session_id for the row.
func ingestText(t *testing.T, s *Store, sourceSession, content string) string {
	t.Helper()
	env := &ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sourceSession,
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Now().UTC(),
		Cwd:             "/work/" + sourceSession,
		ContentText:     content,
		Payload:         map[string]any{},
		Transport:       "hook",
		Redaction:       &ingest.Redaction{Applied: true},
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := IngestEnvelope(t.Context(), tx, env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("IngestEnvelope: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return ingest.DeriveSessionID("claude-code", sourceSession)
}

// matchCount returns how many events_fts rows match the given FTS5
// MATCH expression. Caller is responsible for FTS5-safe quoting; this
// helper keeps the assertions in the test bodies short.
func matchCount(t *testing.T, s *Store, expr string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM events_fts WHERE events_fts MATCH ?`, expr,
	).Scan(&n); err != nil {
		t.Fatalf("FTS count for %q: %v", expr, err)
	}
	return n
}

// TestFTSTokenizer_SeparatorsSplitIdentifiers confirms the migrated
// tokenizer treats `_`, `-`, `.`, `/` as separators rather than word
// characters. Pre-migration, these all lived inside one giant token
// and were unfindable except by typing the entire string.
func TestFTSTokenizer_SeparatorsSplitIdentifiers(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ingestText(t, s, "sess-paths", "fix internal/store/migrate.go panic")
	ingestText(t, s, "sess-snake", "the session_id field is a UUIDv5")
	ingestText(t, s, "sess-dash", "deploy claude-code via systemd")

	for _, tc := range []struct {
		name string
		expr string
		want int
	}{
		{"slash-split: store finds path content", "store", 1},
		{"slash-split: migrate finds path content", "migrate", 1},
		{"dot-split: go finds .go suffix", "go", 1},
		{"underscore-split: session finds session_id", "session", 1},
		{"underscore-split: id finds session_id", "id", 1},
		{"dash-split: claude finds claude-code", "claude", 1},
		{"dash-split: code finds claude-code", "code", 1},
		{"phrase reassembles split tokens", `"internal store migrate go"`, 1},
		{"prefix on split token", "migrat*", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := matchCount(t, s, tc.expr); got != tc.want {
				t.Errorf("MATCH %s: got %d hits, want %d", tc.expr, got, tc.want)
			}
		})
	}
}

// TestFTSTokenizer_NoStemmingByDefault confirms porter is gone:
// `migrate` and `migrating` are now distinct tokens, found only by
// typing the actual word (or a prefix of it via internal/searchquery).
//
// This is intentional — code identifiers carry meaning in their
// exact spelling. Prefix matching via the searchquery layer covers
// the natural-language ergonomic ("shutdown" → "shutdowns").
func TestFTSTokenizer_NoStemmingByDefault(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ingestText(t, s, "sess-stem", "the migration script ran")

	// `migrate` (the porter stem of `migration`) must NOT match
	// `migration` directly. With porter, both stemmed to `migrat` and
	// would collide; without porter, they're distinct.
	if got := matchCount(t, s, "migrate"); got != 0 {
		t.Errorf("MATCH migrate: got %d hits, want 0 (no stemming)", got)
	}

	// `migration` finds itself.
	if got := matchCount(t, s, "migration"); got != 1 {
		t.Errorf("MATCH migration: got %d hits, want 1", got)
	}

	// And prefix matching gives us the natural-language ergonomic.
	if got := matchCount(t, s, "migrat*"); got != 1 {
		t.Errorf("MATCH migrat*: got %d hits, want 1", got)
	}
}

// TestFTSTokenizer_BackfilledFromEvents verifies the migration's
// backfill INSERT actually populated events_fts with whatever was
// already in events. We can't easily simulate "DB at v3, then
// migrate" here (Open always runs the full chain), but we can
// confirm the steady-state invariant: every row in events appears
// in events_fts with matching rowid + content.
func TestFTSTokenizer_BackfilledFromEvents(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	for i := 0; i < 5; i++ {
		ingestText(t, s, "sess-fill", "row "+string(rune('a'+i))+" content")
	}

	var eventCount, ftsCount int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&eventCount); err != nil {
		t.Fatalf("events count: %v", err)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM events_fts`).Scan(&ftsCount); err != nil {
		t.Fatalf("events_fts count: %v", err)
	}
	if eventCount != ftsCount {
		t.Errorf("row count mismatch: events=%d events_fts=%d", eventCount, ftsCount)
	}

	// Spot-check a content match still works after several inserts.
	if got := matchCount(t, s, "content"); got != 5 {
		t.Errorf("MATCH content: got %d, want 5", got)
	}
}

// TestFTSTokenizer_ReopenIsClean confirms that opening an existing
// store re-runs the migrate path idempotently — schema_version is
// still 4 and FTS still works.
func TestFTSTokenizer_ReopenIsClean(t *testing.T) {
	t.Parallel()
	path := tempStorePath(t)
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	ingestText(t, s1, "sess-reopen", "deploy migrate.go fix")
	_ = s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = s2.Close() }()

	var v string
	if err := s2.DB().QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != "12" {
		t.Errorf("schema_version after reopen: %q want 12", v)
	}
	if got := matchCount(t, s2, "migrate"); got != 1 {
		t.Errorf("MATCH migrate after reopen: got %d, want 1", got)
	}
}

// ingestSubagentEvent ingests one event with explicit subagent
// fields. Returns the derived session_id. Used by tests that need
// to populate the subagent_id / subagent_type columns directly.
func ingestSubagentEvent(t *testing.T, s *Store, sourceSession, content, subID, subType string, ts time.Time) string {
	t.Helper()
	env := &ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sourceSession,
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        ts.UTC(),
		Cwd:             "/work/" + sourceSession,
		ContentText:     content,
		Payload:         map[string]any{},
		Subagent:        &ingest.Subagent{ID: subID, Type: subType},
		Transport:       "hook",
		Redaction:       &ingest.Redaction{Applied: true},
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := IngestEnvelope(t.Context(), tx, env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("IngestEnvelope: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return ingest.DeriveSessionID("claude-code", sourceSession)
}

// TestLoadSubagentSpans_DoesNotFragmentOnTypeChange pins the B2
// audit fix: a host reporting two different subagent_type values
// for the same subagent_id must surface as ONE thread with the
// most-recent type, not two phantom threads.
func TestLoadSubagentSpans_DoesNotFragmentOnTypeChange(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	const subID = "agent-7"
	ingestSubagentEvent(t, s, "sess-frag", "step one", subID, "planner", now)
	ingestSubagentEvent(t, s, "sess-frag", "step two", subID, "research-planner", now.Add(time.Second))

	spans, err := LoadSubagentSpans(t.Context(), s.DB(), "", 10)
	if err != nil {
		t.Fatalf("LoadSubagentSpans: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1 (subagent_type change must not fragment)", len(spans))
	}
	if spans[0].SubagentID != subID {
		t.Errorf("SubagentID: got %q, want %q", spans[0].SubagentID, subID)
	}
	if spans[0].EventCount != 2 {
		t.Errorf("EventCount: got %d, want 2", spans[0].EventCount)
	}
}

// tempStorePath returns a per-test path inside t.TempDir() suitable
// for two-phase Open/Close/Open tests where openTemp's t.Cleanup
// would close the store before the second Open.
func tempStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "store.db")
}
