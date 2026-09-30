package store

import (
	"context"
	"testing"

	"github.com/toabctl/aichronicles/internal/events"
)

func ingestOne(t *testing.T, s *Store) string {
	t.Helper()
	env, raw := newValidEnvelope(t)
	if _, err := NewSink(s).Write(context.Background(), events.Event{Envelope: env, Raw: raw}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return events.DeriveSessionID(env.SourceAgent, env.SourceSessionID)
}

// TestLoadSessionDigest_LatestSummaryTieIsDeterministic pins the
// "latest summary" pick when two summaries share created_at_ms: the
// subquery ordered by created_at_ms alone, so SQLite could return
// either body. The later-inserted (higher id) row must win.
func TestLoadSessionDigest_LatestSummaryTieIsDeterministic(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	id := ingestOne(t, s)
	const ts = 1_760_000_000_000
	for _, body := range []string{`{"topic":"first"}`, `{"topic":"second"}`} {
		if _, err := s.DB().Exec(
			`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms)
			 VALUES (?, 'summary', ?, ?, 'm', ?)`, id, body, "h"+body, ts); err != nil {
			t.Fatal(err)
		}
	}
	row, err := LoadSessionDigest(context.Background(), s.DB(), id)
	if err != nil || row == nil {
		t.Fatalf("load: %v %v", row, err)
	}
	if row.LatestSummary == nil || *row.LatestSummary != `{"topic":"second"}` {
		t.Errorf("latest summary: got %v, want the higher-id body", row.LatestSummary)
	}
}

// TestLoadSessionsMissingSummary_FullRowShape pins that the
// missing-summary sweep returns the shared digest projection (it
// used to drop source_agent, start_cwd and event_count).
func TestLoadSessionsMissingSummary_FullRowShape(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	id := ingestOne(t, s)
	rows, err := LoadSessionsMissingSummary(context.Background(), s.DB(), 1, SessionFilter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("rows: %+v", rows)
	}
	r := rows[0]
	if r.EventCount != 1 || r.SourceAgent == "" || r.SourceSessionID == "" {
		t.Errorf("missing-summary row lacks stored columns: %+v", r)
	}
	if r.LatestSummary != nil || r.SummaryTopic != nil {
		t.Errorf("missing-summary row claims a summary: %+v", r)
	}
}
