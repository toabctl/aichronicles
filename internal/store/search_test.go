package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

// contains is a one-letter alias for strings.Contains, kept to
// shorten the per-row assertions in the table-driven tests above.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// ingestForSearch is the multi-arg cousin of ingestText that lets
// tests pin transport and timestamp explicitly. Returns the derived
// session_id so callers can filter by it.
func ingestForSearch(t *testing.T, s *Store, sourceSession, kind, content, transport string, ts time.Time) string {
	t.Helper()
	env := &ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sourceSession,
		Kind:            kind,
		Role:            "user",
		TsSource:        ts.UTC(),
		Cwd:             "/work/" + sourceSession,
		ContentText:     content,
		Payload:         map[string]any{},
		Transport:       transport,
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

func TestSearchEvents_FindsByKeyword(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ingestForSearch(t, s, "sess-foo", "user_prompt", "what is jsonl format", "hook", now)
	ingestForSearch(t, s, "sess-bar", "user_prompt", "explain systemd socket activation", "hook", now)

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{Query: "jsonl"})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits: got %d, want 1", len(hits))
	}
	if hits[0].Content.String != "what is jsonl format" {
		t.Errorf("content: got %q", hits[0].Content.String)
	}
}

func TestSearchEvents_RejectsEmptyQuery(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	_, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{Query: "  "})
	if err == nil {
		t.Fatal("expected error for whitespace-only query")
	}
}

func TestSearchEvents_RespectsKindFilter(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ingestForSearch(t, s, "sess-mix", "user_prompt", "alpha shared", "hook", now)
	ingestForSearch(t, s, "sess-mix", "tool_use", "alpha shared", "hook", now.Add(time.Second))

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query: "alpha", Kind: "user_prompt",
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits: got %d, want 1", len(hits))
	}
	if hits[0].Kind != "user_prompt" {
		t.Errorf("kind: got %q", hits[0].Kind)
	}
}

func TestSearchEvents_RespectsSessionFilter(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	fooID := ingestForSearch(t, s, "sess-foo", "user_prompt", "shared marker", "hook", now)
	ingestForSearch(t, s, "sess-bar", "user_prompt", "shared marker", "hook", now)

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query: "marker", SessionID: fooID,
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits: got %d, want 1", len(hits))
	}
	if hits[0].SessionID != fooID {
		t.Errorf("session_id: got %q", hits[0].SessionID)
	}
}

func TestSearchEvents_RespectsSinceFilter(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ingestForSearch(t, s, "sess-old", "user_prompt", "ancient marker", "hook", old)
	ingestForSearch(t, s, "sess-new", "user_prompt", "fresh marker", "hook", recent)

	cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query: "marker", SinceMs: cutoff,
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits: got %d, want 1", len(hits))
	}
	if hits[0].Content.String != "fresh marker" {
		t.Errorf("expected fresh marker, got %q", hits[0].Content.String)
	}
}

func TestSearchEvents_DefaultLimitIsTwenty(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		ingestForSearch(t, s, "sess-bulk", "user_prompt",
			"limittest "+string(rune('a'+i%26))+string(rune('a'+i/26)),
			"hook", now.Add(time.Duration(i)*time.Second))
	}

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{Query: "limittest"})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 20 {
		t.Errorf("default limit: got %d hits, want 20", len(hits))
	}
}

func TestSearchEvents_ExplicitLimit(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		ingestForSearch(t, s, "sess-bulk", "user_prompt",
			"limittest variant "+string(rune('a'+i)),
			"hook", now.Add(time.Duration(i)*time.Second))
	}

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query: "limittest", Limit: 3,
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 3 {
		t.Errorf("hits: got %d, want 3", len(hits))
	}
}

func TestSearchEvents_DedupCollapsesDuplicateTurn(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	// Two envelopes for the same turn — one hook-captured, one
	// transcript-imported. Same source_session_id, role, kind, and
	// content. Different transports, different ts_source.
	ingestForSearch(t, s, "sess-dup", "user_prompt", "duplicated turn marker", "hook", now)
	ingestForSearch(t, s, "sess-dup", "user_prompt", "duplicated turn marker", "import", now.Add(50*time.Millisecond))

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{Query: "duplicated"})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("dedup: got %d hits, want 1", len(hits))
	}
	// Hook event was at exactly :00.000; import was 50ms later.
	// Dedup must pick the hook row.
	if hits[0].TsSourceMs%1000 != 0 {
		t.Errorf("dedup picked import row (ts_ms=%d); hook expected", hits[0].TsSourceMs)
	}
}

func TestSearchEvents_NoDedupSurfacesBoth(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ingestForSearch(t, s, "sess-dup", "user_prompt", "duplicated turn marker", "hook", now)
	ingestForSearch(t, s, "sess-dup", "user_prompt", "duplicated turn marker", "import", now.Add(50*time.Millisecond))

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query: "duplicated", NoDedup: true,
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("NoDedup: got %d, want 2", len(hits))
	}
}

// TestSearchEvents_TrigramFallbackOnSubstring verifies that a query
// for which the primary unicode61 index has no hits falls through
// to the trigram index. Typing `MongoD` with the searchquery prefix
// `MongoD*` finds nothing in the unicode61 word index (MongoDB is
// one whole token, not split into mong+oDB), but the trigram index
// matches because the trigrams of MongoD all appear inside MongoDB.
func TestSearchEvents_TrigramFallbackOnSubstring(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ingestForSearch(t, s, "sess-mongo", "user_prompt",
		"shutting down MongoDB cleanly is non-trivial", "hook", now)

	// `MongoD*` against unicode61 returns nothing — MongoDB is one
	// token and the prefix `MongoD` doesn't match the token (FTS5's
	// prefix `*` requires a token boundary, not a substring).
	// Trigram catches it.
	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{Query: "MongoD*"})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("trigram fallback: got %d hits, want 1", len(hits))
	}
	if !contains(hits[0].Content.String, "MongoDB") {
		t.Errorf("expected MongoDB in content, got %q", hits[0].Content.String)
	}
}

// TestSearchEvents_PrimaryWinsWhenItHasHits proves the fallback
// only fires when the primary returns nothing — we don't pay the
// trigram lookup cost when the primary already answered, and we
// don't accidentally merge results from both indexes.
//
// Setup pins this: row A contains `mongo` as a whole word, row B
// contains `mongoDB` (one whole token). A literal-token query for
// `mongo` matches only row A on the primary unicode61 index but
// matches BOTH rows on the trigram index (the trigrams of "mongo"
// all appear inside "mongoDB"). If we returned just the primary's
// hit (1 row, A), the fallback was correctly skipped. If we got 2
// rows back, the fallback ran and merged.
func TestSearchEvents_PrimaryWinsWhenItHasHits(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ingestForSearch(t, s, "sess-pri-a", "user_prompt", "I love mongo", "hook", now)
	ingestForSearch(t, s, "sess-pri-b", "user_prompt", "deploying mongoDB", "hook", now.Add(time.Second))

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{Query: "mongo"})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("primary path: got %d hits, want 1 (trigram fallback should NOT have run)", len(hits))
	}
	if !contains(hits[0].Content.String, "I love mongo") {
		t.Errorf("expected primary's row, got %q", hits[0].Content.String)
	}
}

// TestSearchEvents_TrigramFallbackHonorsFilters confirms that the
// fallback runs the same SearchEventOpts (kind, session, since,
// limit) as the primary; the user shouldn't see filter changes when
// the index path silently switches.
func TestSearchEvents_TrigramFallbackHonorsFilters(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	fooID := ingestForSearch(t, s, "sess-foo", "user_prompt",
		"shutting down MongoDB cleanly", "hook", now)
	ingestForSearch(t, s, "sess-bar", "user_prompt",
		"shutting down MongoDB cleanly", "hook", now)

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query:     "MongoD*",
		SessionID: fooID,
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("trigram + filter: got %d hits, want 1", len(hits))
	}
	if hits[0].SessionID != fooID {
		t.Errorf("session filter leaked: got %q, want %q", hits[0].SessionID, fooID)
	}
}

func TestSearchEvents_OrderRecencyReturnsNewestFirst(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ingestForSearch(t, s, "sess-recency", "user_prompt", "recencytest old", "hook", older)
	ingestForSearch(t, s, "sess-recency", "user_prompt", "recencytest new", "hook", newer)

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query: "recencytest", Order: OrderRecency,
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits: got %d, want 2", len(hits))
	}
	if hits[0].TsSourceMs <= hits[1].TsSourceMs {
		t.Errorf("OrderRecency: expected newer first, got ts[%d, %d]",
			hits[0].TsSourceMs, hits[1].TsSourceMs)
	}
}
