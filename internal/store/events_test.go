package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/ingest"
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
