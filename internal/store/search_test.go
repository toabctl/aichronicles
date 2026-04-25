package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

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
