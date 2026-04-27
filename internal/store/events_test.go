package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

// seedEvents inserts n events for one session via the normal ingest
// path so triggers (sessions, events_fts) run as they would in prod.
// Events are spaced 1ms apart starting from baseTs.
func seedEvents(t *testing.T, s *Store, sessionKey string, n int, baseTs time.Time) {
	t.Helper()
	for i := range n {
		env := &ingest.Envelope{
			V:               1,
			EventID:         uuid.Must(uuid.NewV7()).String(),
			SourceAgent:     "claude-code",
			SourceSessionID: sessionKey,
			Kind:            "user_prompt",
			Role:            "user",
			TsSource:        baseTs.Add(time.Duration(i) * time.Millisecond),
			Payload:         map[string]any{"i": i},
			Redaction:       &ingest.Redaction{Applied: true},
		}
		raw := []byte(`{"v":1}`) // body content does not matter for these tests
		withTx(t, s, func(tx *sql.Tx) {
			if _, err := IngestEnvelope(t.Context(), tx, env, raw, env.TsSource.UnixMilli()); err != nil {
				t.Fatalf("seed ingest: %v", err)
			}
		})
	}
}

func TestLoadEventsForSession_ClampsToDefaultLimit(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Seed one row above the default cap to prove clamping actually bites.
	total := DefaultEventsPerSessionLimit + 5
	seedEvents(t, s, "big-session", total, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	sessionID := ingest.DeriveSessionID("claude-code", "big-session")
	got, err := LoadEventsForSession(t.Context(), s.DB(), sessionID, 0) // 0 → default cap
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != DefaultEventsPerSessionLimit {
		t.Errorf("default cap: got %d rows, want %d", len(got), DefaultEventsPerSessionLimit)
	}
}

func TestLoadEventsForSession_ExplicitLimitWins(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	seedEvents(t, s, "sess-1", 10, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	sessionID := ingest.DeriveSessionID("claude-code", "sess-1")
	got, err := LoadEventsForSession(t.Context(), s.DB(), sessionID, 3)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("explicit limit: got %d rows, want 3", len(got))
	}
	// Oldest-first ordering preserved under LIMIT.
	for i := 1; i < len(got); i++ {
		if got[i].TsSourceMs < got[i-1].TsSourceMs {
			t.Errorf("ordering broken: row %d (%d) < row %d (%d)",
				i, got[i].TsSourceMs, i-1, got[i-1].TsSourceMs)
		}
	}
}

func TestLoadEventsForSession_UnknownSession_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	got, err := LoadEventsForSession(t.Context(), s.DB(), "nope", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d rows", len(got))
	}
}

func TestLoadExtractionsForSession_DedupsAndOrdersByFirstSight(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Seed one event so sessions + events exist, then insert
	// extractions directly — keeps the test focused on the read
	// path rather than exercising the ingest extractor.
	seedEvents(t, s, "extract-test", 1, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sessionID := ingest.DeriveSessionID("claude-code", "extract-test")

	// Grab the event_id we just inserted.
	var evID string
	if err := s.DB().QueryRow(
		`SELECT event_id FROM events WHERE session_id = ?`, sessionID,
	).Scan(&evID); err != nil {
		t.Fatalf("fetch event_id: %v", err)
	}

	// Insert URLs out of order, with duplicates interleaved.
	inserts := []struct {
		kind, value string
	}{
		{"url", "https://first.example/"},
		{"url", "https://second.example/"},
		{"url", "https://first.example/"}, // duplicate
		{"url", "https://third.example/"},
		{"file_path", "/some/path"}, // different kind
	}
	for _, r := range inserts {
		if _, err := s.DB().Exec(
			`INSERT INTO extractions(event_id, session_id, kind, value) VALUES (?, ?, ?, ?)`,
			evID, sessionID, r.kind, r.value,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	got, err := LoadExtractionsForSession(t.Context(), s.DB(), sessionID, "url")
	if err != nil {
		t.Fatalf("LoadExtractionsForSession: %v", err)
	}
	want := []Extraction{
		{Kind: "url", Value: "https://first.example/"},
		{Kind: "url", Value: "https://second.example/"},
		{Kind: "url", Value: "https://third.example/"},
	}
	if len(got) != len(want) {
		t.Fatalf("count: got %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLoadExtractionsForSession_EmptyKindIsError(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	if _, err := LoadExtractionsForSession(t.Context(), s.DB(), "whatever", ""); err == nil {
		t.Fatal("expected error for empty kind")
	}
}

func TestLoadExtractionsForSession_UnknownSessionReturnsEmpty(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	got, err := LoadExtractionsForSession(t.Context(), s.DB(), "no-such-session", "url")
	if err != nil {
		t.Fatalf("LoadExtractionsForSession: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %+v", got)
	}
}

func TestResolveSessionIDPrefix_ExactMatch(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	seedEvents(t, s, "exact-test", 1, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	want := ingest.DeriveSessionID("claude-code", "exact-test")

	got, err := ResolveSessionIDPrefix(t.Context(), s.DB(), want)
	if err != nil {
		t.Fatalf("resolve full: %v", err)
	}
	if got != want {
		t.Errorf("full id: got %q, want %q", got, want)
	}
}

func TestResolveSessionIDPrefix_UniquePrefixResolves(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	seedEvents(t, s, "prefix-test", 1, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	full := ingest.DeriveSessionID("claude-code", "prefix-test")

	// The 8-char preview is the common case — that's what
	// `aichronicles sessions` prints.
	got, err := ResolveSessionIDPrefix(t.Context(), s.DB(), full[:8])
	if err != nil {
		t.Fatalf("resolve prefix: %v", err)
	}
	if got != full {
		t.Errorf("prefix expanded to %q, want %q", got, full)
	}
}

func TestResolveSessionIDPrefix_NoMatchErrs(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	_, err := ResolveSessionIDPrefix(t.Context(), s.DB(), "deadbeef")
	if !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("want ErrNoSuchSession, got %v", err)
	}
}

func TestResolveSessionIDPrefix_AmbiguousLists(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	// DeriveSessionID is UUIDv5 so collisions are impossible in
	// normal use, but we can contrive two ids sharing a prefix by
	// inserting directly.
	for _, id := range []string{
		"deadbeef-0000-0000-0000-000000000001",
		"deadbeef-0000-0000-0000-000000000002",
	} {
		if _, err := s.DB().Exec(
			`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, 'claude-code', ?)`,
			id, id,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	_, err := ResolveSessionIDPrefix(t.Context(), s.DB(), "deadbeef")
	if !errors.Is(err, ErrAmbiguousSessionPrefix) {
		t.Errorf("want ErrAmbiguousSessionPrefix, got %v", err)
	}
	// Both matching ids should appear in the message so the user
	// can pick one.
	if !strings.Contains(err.Error(), "000000000001") || !strings.Contains(err.Error(), "000000000002") {
		t.Errorf("ambiguity message should list candidates, got %v", err)
	}
}

func TestResolveSessionIDPrefix_RejectsNonHexInput(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	// Wildcards in the input would be interpreted by LIKE otherwise —
	// the input validator is the defense.
	cases := []string{"1a%febea", "1a_febea", "1a;febea", ""}
	for _, c := range cases {
		if _, err := ResolveSessionIDPrefix(t.Context(), s.DB(), c); err == nil {
			t.Errorf("expected error for input %q", c)
		}
	}
}

func TestResolveSessionIDPrefix_NormalisesCase(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	seedEvents(t, s, "case-test", 1, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	full := ingest.DeriveSessionID("claude-code", "case-test")

	// Uppercase should resolve the same as lowercase.
	got, err := ResolveSessionIDPrefix(t.Context(), s.DB(), strings.ToUpper(full[:8]))
	if err != nil {
		t.Fatalf("resolve upper: %v", err)
	}
	if got != full {
		t.Errorf("got %q, want %q", got, full)
	}
}

func TestLoadEventsForSession_RespectsCancelledContext(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	seedEvents(t, s, "cancel-me", 5, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	// A context that's already dead must never succeed — proves the
	// call path actually uses the context variant rather than
	// quietly dropping it on the floor.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := LoadEventsForSession(ctx, s.DB(), ingest.DeriveSessionID("claude-code", "cancel-me"), 0)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled in error chain, got %v", err)
	}
}

// TestSessionsEffectiveTsIndex_UsedByDigestQuery verifies migration 003's
// expression index actually serves LoadRecentSessionDigests. Without it,
// SQLite full-scans `sessions`; with it, the plan mentions the index by
// name. Catching a silently-dropped index is exactly the kind of thing
// EXPLAIN QUERY PLAN is for.
func TestSessionsEffectiveTsIndex_UsedByDigestQuery(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	rows, err := s.DB().Query(`EXPLAIN QUERY PLAN
		SELECT s.id
		FROM sessions s
		WHERE COALESCE(s.ended_at_ms, s.started_at_ms, 0) >= ?
		ORDER BY COALESCE(s.ended_at_ms, s.started_at_ms, 0) DESC
		LIMIT ?`, 0, 10)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if !strings.Contains(plan.String(), "idx_sessions_effective_ts") {
		t.Errorf("plan does not use idx_sessions_effective_ts:\n%s", plan.String())
	}
}

func TestLoadLatestEventsIndexedByID_EmptyInputReturnsEmptyMap(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	got, err := LoadLatestEventsIndexedByID(t.Context(), s.DB(), nil)
	if err != nil {
		t.Fatalf("nil input: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}

	got, err = LoadLatestEventsIndexedByID(t.Context(), s.DB(), []string{})
	if err != nil {
		t.Fatalf("empty input: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestLoadLatestEventsIndexedByID_LatestWinsPerSession(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Two sessions with multiple events each, plus one session with
	// zero events so we can assert it's absent from the result.
	base := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	seedEvents(t, s, "sess-A", 4, base)
	seedEvents(t, s, "sess-B", 2, base.Add(time.Hour))

	idA := ingest.DeriveSessionID("claude-code", "sess-A")
	idB := ingest.DeriveSessionID("claude-code", "sess-B")
	idMissing := ingest.DeriveSessionID("claude-code", "no-events-here")

	got, err := LoadLatestEventsIndexedByID(t.Context(), s.DB(),
		[]string{idA, idB, idMissing})
	if err != nil {
		t.Fatalf("LoadLatestEventsIndexedByID: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("entry count: got %d, want 2 (A and B; missing has no events)", len(got))
	}

	// seedEvents spaces events 1ms apart starting at baseTs, so the
	// last event for sess-A is at base + 3ms, for sess-B at base+1h+1ms.
	wantA := base.Add(3 * time.Millisecond).UnixMilli()
	wantB := base.Add(time.Hour + 1*time.Millisecond).UnixMilli()
	if a, ok := got[idA]; !ok {
		t.Error("missing sess-A entry")
	} else if a.TsSourceMs != wantA {
		t.Errorf("sess-A latest: got ts %d, want %d", a.TsSourceMs, wantA)
	}
	if b, ok := got[idB]; !ok {
		t.Error("missing sess-B entry")
	} else if b.TsSourceMs != wantB {
		t.Errorf("sess-B latest: got ts %d, want %d", b.TsSourceMs, wantB)
	}
	if _, ok := got[idMissing]; ok {
		t.Error("session with no events should be absent from map")
	}
}

func TestLoadLatestEventsIndexedByID_OneSessionInLargeCohort(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Seed many sessions, ask for one — verify the IN-clause query
	// scopes correctly even with many placeholders.
	base := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	for i := range 5 {
		seedEvents(t, s, "sess-"+string(rune('a'+i)), 3, base.Add(time.Duration(i)*time.Hour))
	}
	target := ingest.DeriveSessionID("claude-code", "sess-c")

	got, err := LoadLatestEventsIndexedByID(t.Context(), s.DB(), []string{target})
	if err != nil {
		t.Fatalf("LoadLatestEventsIndexedByID: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if _, ok := got[target]; !ok {
		t.Error("requested session should be present")
	}
}

func TestLoadSessionsMissingSummary_ExcludesSessionsWithSummary(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	base := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	// Three sessions in the window: A summarized, B and C missing.
	seedEvents(t, s, "sess-A", 1, base)
	seedEvents(t, s, "sess-B", 1, base.Add(time.Hour))
	seedEvents(t, s, "sess-C", 1, base.Add(2*time.Hour))

	idA := ingest.DeriveSessionID("claude-code", "sess-A")
	withTx(t, s, func(tx *sql.Tx) {
		out := &LLMOutput{
			SessionID:   sql.NullString{String: idA, Valid: true},
			Kind:        LLMKindSummary,
			Model:       "test",
			PromptHash:  "hash-A",
			Body:        `{"topic":"A"}`,
			CreatedAtMs: base.UnixMilli(),
		}
		if _, _, err := SaveLLMOutput(t.Context(), tx, out); err != nil {
			t.Fatalf("seed summary: %v", err)
		}
	})

	got, err := LoadSessionsMissingSummary(t.Context(), s.DB(),
		base.Add(-time.Hour).UnixMilli(), SessionFilter{}, 0)
	if err != nil {
		t.Fatalf("LoadSessionsMissingSummary: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 missing-summary rows, got %d", len(got))
	}
	idB := ingest.DeriveSessionID("claude-code", "sess-B")
	idC := ingest.DeriveSessionID("claude-code", "sess-C")
	ids := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !ids[idB] || !ids[idC] {
		t.Errorf("expected sess-B and sess-C in missing set, got %v", ids)
	}
	if ids[idA] {
		t.Errorf("sess-A has a summary; should NOT appear in missing set")
	}
}

func TestLoadSessionsMissingSummary_OrderingNewestFirst(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	base := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	seedEvents(t, s, "old", 1, base)
	seedEvents(t, s, "mid", 1, base.Add(time.Hour))
	seedEvents(t, s, "new", 1, base.Add(2*time.Hour))

	got, err := LoadSessionsMissingSummary(t.Context(), s.DB(),
		base.Add(-time.Hour).UnixMilli(), SessionFilter{}, 0)
	if err != nil {
		t.Fatalf("LoadSessionsMissingSummary: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(got))
	}
	wantOrder := []string{
		ingest.DeriveSessionID("claude-code", "new"),
		ingest.DeriveSessionID("claude-code", "mid"),
		ingest.DeriveSessionID("claude-code", "old"),
	}
	for i, want := range wantOrder {
		if got[i].ID != want {
			t.Errorf("row %d: got %s, want %s", i, got[i].ID, want)
		}
	}
}

func TestLoadSessionsMissingSummary_FilterByCwdAndAgent(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	base := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	seedEvents(t, s, "in-cwd", 1, base)
	seedEvents(t, s, "out-of-cwd", 1, base.Add(time.Hour))

	// Tweak the cwd on one session to test the filter — seedEvents
	// hardcodes "/work/<sessionKey>".
	idIn := ingest.DeriveSessionID("claude-code", "in-cwd")
	if _, err := s.DB().Exec(`UPDATE sessions SET cwd = ? WHERE id = ?`,
		"/devel/target", idIn); err != nil {
		t.Fatalf("set cwd: %v", err)
	}

	got, err := LoadSessionsMissingSummary(t.Context(), s.DB(),
		base.Add(-time.Hour).UnixMilli(),
		SessionFilter{Cwd: "/devel/target"}, 0)
	if err != nil {
		t.Fatalf("LoadSessionsMissingSummary: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("cwd filter: got %d rows, want 1", len(got))
	}
	if got[0].ID != idIn {
		t.Errorf("got %s, want %s", got[0].ID, idIn)
	}

	// Agent filter — every fixture is "claude-code" so this should
	// match everything in the window; a bogus agent should match
	// nothing.
	all, _ := LoadSessionsMissingSummary(t.Context(), s.DB(),
		base.Add(-time.Hour).UnixMilli(),
		SessionFilter{Agent: "claude-code"}, 0)
	if len(all) != 2 {
		t.Errorf("agent=claude-code: got %d, want 2", len(all))
	}
	none, _ := LoadSessionsMissingSummary(t.Context(), s.DB(),
		base.Add(-time.Hour).UnixMilli(),
		SessionFilter{Agent: "no-such-agent"}, 0)
	if len(none) != 0 {
		t.Errorf("bogus agent: got %d, want 0", len(none))
	}
}

func TestLoadSessionsMissingSummary_LimitClamps(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	base := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	for i := range 5 {
		seedEvents(t, s, "sess-"+string(rune('a'+i)), 1, base.Add(time.Duration(i)*time.Hour))
	}

	got, err := LoadSessionsMissingSummary(t.Context(), s.DB(),
		base.Add(-time.Hour).UnixMilli(), SessionFilter{}, 2)
	if err != nil {
		t.Fatalf("LoadSessionsMissingSummary: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("explicit limit: got %d rows, want 2", len(got))
	}
}
