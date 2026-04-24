package store

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/ingest"
)

// newValidEnvelope returns an Envelope that passes Validate and is
// suitable for IngestEnvelope. Tests mutate it where needed.
func newValidEnvelope(t *testing.T) (*ingest.Envelope, []byte) {
	t.Helper()
	env := &ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-a",
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC),
		Cwd:             "/tmp/proj",
		ContentText:     "hello from ingest test",
		Payload:         map[string]any{"prompt": "hello from ingest test"},
		Transport:       "hook",
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return env, raw
}

func withTx(t *testing.T, s *Store, fn func(tx *sql.Tx)) {
	t.Helper()
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	fn(tx)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestIngestEnvelope_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, raw := newValidEnvelope(t)

	var deduped bool
	withTx(t, s, func(tx *sql.Tx) {
		d, err := IngestEnvelope(tx, env, raw, 99)
		if err != nil {
			t.Fatalf("IngestEnvelope: %v", err)
		}
		deduped = d
	})

	if deduped {
		t.Error("first insert should not report deduped")
	}

	// raw exists with our envelope_json and correct ts_server_ms
	var gotJSON string
	var tsServer int64
	err := s.DB().QueryRow(
		`SELECT envelope_json, ts_server_ms FROM raw_envelopes WHERE event_id=?`, env.EventID,
	).Scan(&gotJSON, &tsServer)
	if err != nil {
		t.Fatalf("raw row: %v", err)
	}
	if gotJSON != string(raw) {
		t.Errorf("envelope_json not preserved verbatim")
	}
	if tsServer != 99 {
		t.Errorf("ts_server_ms: got %d, want 99", tsServer)
	}

	// session row exists with derived id and matches aggregates
	sessionID := ResolveSessionID(env.SourceAgent, env.SourceSessionID)
	var cnt int
	var cwd string
	var startMs, endMs int64
	err = s.DB().QueryRow(
		`SELECT event_count, cwd, started_at_ms, ended_at_ms FROM sessions WHERE id=?`,
		sessionID,
	).Scan(&cnt, &cwd, &startMs, &endMs)
	if err != nil {
		t.Fatalf("session row: %v", err)
	}
	if cnt != 1 {
		t.Errorf("event_count: got %d, want 1", cnt)
	}
	if cwd != "/tmp/proj" {
		t.Errorf("cwd: got %q", cwd)
	}
	wantMs := env.TsSource.UnixMilli()
	if startMs != wantMs || endMs != wantMs {
		t.Errorf("ts bounds: got [%d,%d], want both %d", startMs, endMs, wantMs)
	}

	// event row has the right session_id + kind + content
	var gotSession, gotKind, gotContent string
	err = s.DB().QueryRow(
		`SELECT session_id, kind, content_text FROM events WHERE event_id=?`, env.EventID,
	).Scan(&gotSession, &gotKind, &gotContent)
	if err != nil {
		t.Fatalf("event row: %v", err)
	}
	if gotSession != sessionID {
		t.Errorf("session_id: got %q, want %q", gotSession, sessionID)
	}
	if gotKind != "user_prompt" {
		t.Errorf("kind: got %q", gotKind)
	}
	if gotContent != "hello from ingest test" {
		t.Errorf("content_text: got %q", gotContent)
	}

	// FTS should match
	var n int
	_ = s.DB().QueryRow(
		`SELECT COUNT(*) FROM events_fts WHERE events_fts MATCH ?`, "hello",
	).Scan(&n)
	if n != 1 {
		t.Errorf("FTS missing the event: got %d", n)
	}
}

func TestIngestEnvelope_DuplicateIsDedupedWithoutTouching(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, raw := newValidEnvelope(t)

	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(tx, env, raw, 1); err != nil {
			t.Fatalf("first: %v", err)
		}
	})

	var firstCount int
	_ = s.DB().QueryRow(`SELECT event_count FROM sessions`).Scan(&firstCount)

	withTx(t, s, func(tx *sql.Tx) {
		d, err := IngestEnvelope(tx, env, raw, 2)
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		if !d {
			t.Error("second insert should report deduped")
		}
	})

	var secondCount int
	_ = s.DB().QueryRow(`SELECT event_count FROM sessions`).Scan(&secondCount)
	if secondCount != firstCount {
		t.Errorf("event_count changed on dedup: %d -> %d", firstCount, secondCount)
	}

	// Raw row count should still be 1 — no duplicate row
	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM raw_envelopes`).Scan(&n)
	if n != 1 {
		t.Errorf("raw_envelopes count: got %d, want 1", n)
	}
}

func TestIngestEnvelope_MultipleEventsOneSessionAggregates(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	for i := 0; i < 5; i++ {
		env, raw := newValidEnvelope(t)
		env.EventID = uuid.Must(uuid.NewV7()).String()
		env.TsSource = env.TsSource.Add(time.Duration(i) * time.Minute)
		// keep same (SourceAgent, SourceSessionID) → same session
		withTx(t, s, func(tx *sql.Tx) {
			if _, err := IngestEnvelope(tx, env, raw, int64(1000+i)); err != nil {
				t.Fatalf("ingest %d: %v", i, err)
			}
		})
	}

	var cnt int
	_ = s.DB().QueryRow(`SELECT event_count FROM sessions`).Scan(&cnt)
	if cnt != 5 {
		t.Errorf("event_count: got %d, want 5", cnt)
	}
}

func TestIngestEnvelope_ToolFieldsPersist(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, raw := newValidEnvelope(t)
	env.Kind = "tool_use"
	env.Role = "tool"
	env.Tool = &ingest.Tool{Name: "Bash", NameRaw: "Bash", CallID: "toolu_abc"}

	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(tx, env, raw, 1); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	})

	var name, callID string
	err := s.DB().QueryRow(
		`SELECT tool_name, tool_call_id FROM events WHERE event_id=?`, env.EventID,
	).Scan(&name, &callID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "Bash" || callID != "toolu_abc" {
		t.Errorf("tool fields: got %q/%q", name, callID)
	}
}

func TestIngestEnvelope_NilReturnsError(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	withTx(t, s, func(tx *sql.Tx) {
		_, err := IngestEnvelope(tx, nil, nil, 0)
		if err == nil {
			t.Error("expected error for nil envelope")
		}
	})
}

func TestIngestEnvelope_DifferentSessionsCoexist(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	for i, sessID := range []string{"sess-a", "sess-b", "sess-c"} {
		env, raw := newValidEnvelope(t)
		env.EventID = uuid.Must(uuid.NewV7()).String()
		env.SourceSessionID = sessID
		withTx(t, s, func(tx *sql.Tx) {
			if _, err := IngestEnvelope(tx, env, raw, int64(i)); err != nil {
				t.Fatalf("ingest %s: %v", sessID, err)
			}
		})
	}

	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 3 {
		t.Errorf("sessions count: got %d, want 3", n)
	}
}
