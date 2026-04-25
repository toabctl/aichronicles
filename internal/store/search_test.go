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

// ingestFileTouch ingests a tool_use envelope for a file-touching
// tool (Read/Write/Edit/NotebookEdit). Returns the derived
// session_id. The point of this helper is to seed an `extractions`
// row of kind=file_path WITHOUT the path appearing in content_text,
// so the extractions_fts fallback can be exercised in isolation.
func ingestFileTouch(t *testing.T, s *Store, sourceSession, toolName, filePath, content string, ts time.Time) string {
	t.Helper()
	env := &ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sourceSession,
		Kind:            "tool_use",
		Role:            "tool",
		TsSource:        ts.UTC(),
		Cwd:             "/work/" + sourceSession,
		ContentText:     content,
		Tool:            &ingest.Tool{Name: toolName},
		Payload: map[string]any{
			"tool_input": map[string]any{"file_path": filePath},
		},
		Transport: "hook",
		Redaction: &ingest.Redaction{Applied: true},
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

// TestSearchEvents_SnippetPopulated proves SQLite's snippet() is
// returned alongside content_text for every FTS hit. The snippet
// should be non-empty and contain the matched term.
func TestSearchEvents_SnippetPopulated(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ingestForSearch(t, s, "sess-snip", "user_prompt",
		"a long preface and then somewhere here is the keyword cluster, plus more text after it",
		"hook", now)

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{Query: "cluster"})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits: got %d, want 1", len(hits))
	}
	if !hits[0].Snippet.Valid || hits[0].Snippet.String == "" {
		t.Fatalf("snippet should be populated, got %+v", hits[0].Snippet)
	}
	if !strings.Contains(hits[0].Snippet.String, "cluster") {
		t.Errorf("snippet should contain matched term, got %q", hits[0].Snippet.String)
	}
}

// TestSearchEvents_SnippetCentersOnMatch verifies that for content
// where the match is far from the start, the snippet shows context
// around the match (not just the first N tokens). The point of
// using SQLite's snippet() over a head-of-content preview.
func TestSearchEvents_SnippetCentersOnMatch(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	prefix := strings.Repeat("alpha beta gamma delta epsilon ", 20) // ~600 chars of filler
	ingestForSearch(t, s, "sess-far", "user_prompt",
		prefix+"the unique payload word is here in the middle "+prefix,
		"hook", now)

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{Query: "payload"})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits: got %d, want 1", len(hits))
	}
	snip := hits[0].Snippet.String
	if !strings.Contains(snip, "payload") {
		t.Errorf("snippet must contain match: %q", snip)
	}
	// The preface is ~600 chars; a head-preview would never reach
	// "payload". Any reasonable snippet length must be << 600.
	if len(snip) > 250 {
		t.Errorf("snippet should be a snippet, not the whole doc (got %d chars)", len(snip))
	}
}

// TestSearchEvents_SnippetEllipsisOnTruncation confirms the `…`
// marker fires when the snippet had to clip context to fit the
// token budget. Both sides are clipped on a center match.
func TestSearchEvents_SnippetEllipsisOnTruncation(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ingestForSearch(t, s, "sess-elip", "user_prompt",
		strings.Repeat("filler ", 50)+"target "+strings.Repeat("filler ", 50),
		"hook", now)

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{Query: "target"})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits: got %d, want 1", len(hits))
	}
	if !strings.Contains(hits[0].Snippet.String, "…") {
		t.Errorf("snippet should carry ellipsis on truncation: %q", hits[0].Snippet.String)
	}
}

// TestSearchEvents_RecencyBoostBreaksTies pins the OrderRank
// behaviour: when two events have identical content (and therefore
// identical bm25 scores), the more recent one ranks first.
func TestSearchEvents_RecencyBoostBreaksTies(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	// Identical content → identical bm25. Recency must pick the winner.
	ingestForSearch(t, s, "sess-old", "user_prompt", "tiebreaker payload here", "hook", older)
	ingestForSearch(t, s, "sess-new", "user_prompt", "tiebreaker payload here", "hook", newer)

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query: "tiebreaker",
		Order: OrderRank,
		NowMs: newer.UnixMilli(), // anchor at the newer event
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits: got %d, want 2", len(hits))
	}
	if hits[0].TsSourceMs <= hits[1].TsSourceMs {
		t.Errorf("recency boost: expected newer first, got ts[%d, %d]",
			hits[0].TsSourceMs, hits[1].TsSourceMs)
	}
}

// TestSearchEvents_RecencyBoostInsideHalfDaysKeepsRelevance proves
// the boost is gentle enough within the recency-half-days window
// that a much-more-relevant slightly-older document still beats a
// barely-relevant new one. Outside that window the boost
// (correctly, by design) starts to dominate — recent work in this
// corpus is the user's likely target — but it shouldn't be doing so
// for last week's results.
//
// Math sanity: at 14 days old the boost factor is 1 + 14/30 ≈ 1.47.
// A bm25 advantage of ~3× clears that ratio comfortably.
func TestSearchEvents_RecencyBoostInsideHalfDaysKeepsRelevance(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	twoWeeksAgo := now.Add(-14 * 24 * time.Hour)

	// "rare" appears 3 times in the older focused row → strong bm25.
	// "rare" appears once buried in noise in the new row → weak score.
	ingestForSearch(t, s, "sess-strong", "user_prompt",
		"rare rare rare keyword in a focused recent document", "hook", twoWeeksAgo)
	ingestForSearch(t, s, "sess-weak", "user_prompt",
		"a long mostly unrelated wall of text "+strings.Repeat("noise ", 30)+"rare just once",
		"hook", now)

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query: "rare",
		Order: OrderRank,
		NowMs: now.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits: got %d, want 2", len(hits))
	}
	if !contains(hits[0].Content.String, "focused recent document") {
		t.Errorf("strong-relevance row 14d old should win, got first hit %q",
			hits[0].Content.String)
	}
}

// TestSearchEvents_OrderRecencyIgnoresRelevance contrasts with the
// boosted OrderRank behaviour: with OrderRecency the bm25 score is
// not consulted at all, so even a less-relevant new event ranks
// before a much-more-relevant old one.
func TestSearchEvents_OrderRecencyIgnoresRelevance(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	ingestForSearch(t, s, "sess-strong", "user_prompt",
		"rare rare keyword in a focused old document", "hook", old)
	ingestForSearch(t, s, "sess-weak", "user_prompt",
		"a long unrelated wall of text "+strings.Repeat("noise ", 30)+"rare just once",
		"hook", now)

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query: "rare",
		Order: OrderRecency,
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits: got %d, want 2", len(hits))
	}
	// New row should come first regardless of its weak relevance.
	if !contains(hits[0].Content.String, "rare just once") {
		t.Errorf("OrderRecency should put new row first, got %q",
			hits[0].Content.String)
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

// TestSearchEvents_ExtractionsFallbackFindsByFilePath proves the
// third-tier fallback fires: an event whose content_text never
// mentions the file path is still findable when the extractions
// table holds it. Pre-fallback this returned no hits.
func TestSearchEvents_ExtractionsFallbackFindsByFilePath(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ingestFileTouch(t, s, "sess-touch", "Read",
		"internal/store/migrate.go",
		"opening that source file", // content_text deliberately path-free
		now)

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query: `"internal/store/migrate.go"`,
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("extractions fallback: got %d hits, want 1", len(hits))
	}
	// Snippet must be the labelled extraction-kind preview, not the
	// content_text — that's how the caller knows it came via the
	// typed-fact path.
	if !strings.HasPrefix(hits[0].Snippet.String, "[file_path] ") {
		t.Errorf("snippet should be labelled with extraction kind, got %q",
			hits[0].Snippet.String)
	}
	if !contains(hits[0].Snippet.String, "internal/store/migrate.go") {
		t.Errorf("snippet should carry the matched extraction value, got %q",
			hits[0].Snippet.String)
	}
}

// TestSearchEvents_ExtractionsFallbackSkippedWhenPrimaryHits confirms
// the fallback is gated on the primary returning zero — when
// content_text already matches, we don't pay the join cost.
func TestSearchEvents_ExtractionsFallbackSkippedWhenPrimaryHits(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	// Row A: content_text mentions the path → primary hits.
	ingestFileTouch(t, s, "sess-pri", "Read",
		"internal/store/migrate.go",
		"opening internal/store/migrate.go for review",
		now)
	// Row B: only the extraction has the path. Would surface only
	// via the fallback. If we returned both, fallback ran; if just
	// A, it didn't.
	ingestFileTouch(t, s, "sess-only-ext", "Read",
		"internal/store/migrate.go",
		"unrelated commentary",
		now.Add(time.Second))

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query: `"internal/store/migrate.go"`,
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("primary should suppress fallback: got %d hits, want 1", len(hits))
	}
	if !contains(hits[0].Content.String, "for review") {
		t.Errorf("expected primary's row, got %q", hits[0].Content.String)
	}
}

// TestSearchEvents_ExtractionsFallbackHonorsFilters keeps the third
// tier consistent with the first two: kind, session, since-ms, and
// limit must still apply when the index path silently switches.
func TestSearchEvents_ExtractionsFallbackHonorsFilters(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	fooID := ingestFileTouch(t, s, "sess-foo", "Read",
		"internal/store/migrate.go", "no path in text", now)
	ingestFileTouch(t, s, "sess-bar", "Read",
		"internal/store/migrate.go", "no path in text", now.Add(time.Second))

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query:     `"internal/store/migrate.go"`,
		SessionID: fooID,
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("extractions fallback + session filter: got %d, want 1", len(hits))
	}
	if hits[0].SessionID != fooID {
		t.Errorf("session filter leaked across extractions path: got %q want %q",
			hits[0].SessionID, fooID)
	}
}

// TestSearchEvents_ExtractionsFallbackDedupsPerEvent makes sure
// multiple extractions on the same event collapse to one row in
// the result. (Today only file_path emits per Read, but URLs and
// shell commands can both produce multiple entries per event.)
func TestSearchEvents_ExtractionsFallbackDedupsPerEvent(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	// Single envelope carrying a Bash tool_use with a command that
	// itself contains a URL — two extractions on the same event:
	// one shell_command, one url. Search for the shared substring
	// the trigram path can't catch (extraction-only fallback).
	env := &ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-multi",
		Kind:            "tool_use",
		Role:            "tool",
		TsSource:        now.UTC(),
		Cwd:             "/work/multi",
		ContentText:     "running a curl",
		Tool:            &ingest.Tool{Name: "Bash"},
		Payload: map[string]any{
			"tool_input": map[string]any{
				"command": "curl https://example.com/uniquefactpath/ok.json",
			},
		},
		Transport: "hook",
		Redaction: &ingest.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(env)
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

	hits, err := SearchEvents(t.Context(), s.DB(), SearchEventOpts{
		Query: "uniquefactpath",
	})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("dedup per event: got %d hits, want 1 (two extractions on one event)",
			len(hits))
	}
}
