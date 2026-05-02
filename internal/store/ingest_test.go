package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/pkg/events"
)

// newValidEnvelope returns an Envelope that passes Validate and is
// suitable for IngestEnvelope. Tests mutate it where needed.
func newValidEnvelope(t *testing.T) (*events.Envelope, []byte) {
	t.Helper()
	env := &events.Envelope{
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
		Redaction:       &events.Redaction{Applied: true},
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

func TestIngestEnvelope_RejectsMissingRedaction(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, raw := newValidEnvelope(t)
	env.Redaction = nil // simulate a caller that forgot to scrub

	withTx(t, s, func(tx *sql.Tx) {
		_, err := IngestEnvelope(t.Context(), tx, env, raw, 99)
		if !errors.Is(err, ErrRedactionRequired) {
			t.Fatalf("expected ErrRedactionRequired, got %v", err)
		}
	})

	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM raw_envelopes`).Scan(&n)
	if n != 0 {
		t.Errorf("raw_envelopes should remain empty, got %d", n)
	}
}

func TestIngestEnvelope_RejectsAppliedFalse(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, raw := newValidEnvelope(t)
	env.Redaction = &events.Redaction{Applied: false}

	withTx(t, s, func(tx *sql.Tx) {
		_, err := IngestEnvelope(t.Context(), tx, env, raw, 99)
		if !errors.Is(err, ErrRedactionRequired) {
			t.Fatalf("expected ErrRedactionRequired, got %v", err)
		}
	})
}

func TestIngestEnvelope_SubagentFieldsRoundTrip(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, raw := newValidEnvelope(t)
	env.Subagent = &events.Subagent{ID: "agent-42", Type: "planner"}
	rawWithSubagent, err := jsonMarshalEnvelope(env)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	_ = raw // ignored — we use the re-marshalled body so envelope_json matches

	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, rawWithSubagent, 1); err != nil {
			t.Fatalf("IngestEnvelope: %v", err)
		}
	})

	var gotID, gotType sql.NullString
	if err := s.DB().QueryRow(
		`SELECT subagent_id, subagent_type FROM events WHERE event_id = ?`, env.EventID,
	).Scan(&gotID, &gotType); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if gotID.String != "agent-42" || !gotID.Valid {
		t.Errorf("subagent_id: got %+v, want agent-42", gotID)
	}
	if gotType.String != "planner" || !gotType.Valid {
		t.Errorf("subagent_type: got %+v, want planner", gotType)
	}
}

func TestIngestEnvelope_TopLevelEventLeavesSubagentNull(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, raw := newValidEnvelope(t)
	// Subagent stays nil — most events.

	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, raw, 1); err != nil {
			t.Fatalf("IngestEnvelope: %v", err)
		}
	})

	var gotID, gotType sql.NullString
	if err := s.DB().QueryRow(
		`SELECT subagent_id, subagent_type FROM events WHERE event_id = ?`, env.EventID,
	).Scan(&gotID, &gotType); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if gotID.Valid {
		t.Errorf("subagent_id should be NULL for top-level events, got %q", gotID.String)
	}
	if gotType.Valid {
		t.Errorf("subagent_type should be NULL for top-level events, got %q", gotType.String)
	}
}

// jsonMarshalEnvelope re-serialises an envelope after a test mutation
// so envelope_json stored alongside the row matches the typed columns
// extracted from the same struct.
func jsonMarshalEnvelope(env *events.Envelope) ([]byte, error) {
	return json.Marshal(env)
}

// TestIngestEnvelope_ConcurrentAllocatesUniqueSeqs pins the B3
// audit fix: concurrent ingests against the same store must each
// receive a unique ingest_seq. Pre-fix, two transactions could
// both compute the same MAX(ingest_seq)+1 from their snapshots,
// the second's INSERT would hit the UNIQUE constraint and INSERT
// OR IGNORE would silently drop it — the caller saw "deduped"
// even though no event_id collision occurred.
//
// This test runs N parallel goroutines each ingesting one
// envelope. After they complete, every envelope must be in
// raw_envelopes (no silent drops) and every ingest_seq must be
// unique (the schema's UNIQUE constraint enforces that, but we
// also assert n distinct seqs to make the contract loud).
func TestIngestEnvelope_ConcurrentAllocatesUniqueSeqs(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const n = 20
	envs := make([]*events.Envelope, n)
	raws := make([][]byte, n)
	for i := 0; i < n; i++ {
		envs[i], raws[i] = newValidEnvelope(t)
		envs[i].SourceSessionID = "concurrent-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		// Re-marshal so envelope_json carries the unique
		// SourceSessionID; otherwise dedup folds them.
		var err error
		raws[i], err = json.Marshal(envs[i])
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
	}

	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			tx, err := s.DB().Begin()
			if err != nil {
				errs <- err
				return
			}
			if _, err := IngestEnvelope(t.Context(), tx, envs[i], raws[i], int64(i)); err != nil {
				_ = tx.Rollback()
				errs <- err
				return
			}
			if err := tx.Commit(); err != nil {
				errs <- err
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	var stored, distinctSeqs int
	if err := s.DB().QueryRow(`SELECT COUNT(*), COUNT(DISTINCT ingest_seq) FROM raw_envelopes`).
		Scan(&stored, &distinctSeqs); err != nil {
		t.Fatalf("count: %v", err)
	}
	if stored != n {
		t.Errorf("stored: got %d, want %d (events were silently dropped)", stored, n)
	}
	if distinctSeqs != n {
		t.Errorf("distinct ingest_seqs: got %d, want %d", distinctSeqs, n)
	}
}

func TestIngestEnvelope_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, raw := newValidEnvelope(t)

	var deduped bool
	withTx(t, s, func(tx *sql.Tx) {
		d, err := IngestEnvelope(t.Context(), tx, env, raw, 99)
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

func TestIngestEnvelope_PersistsTransportColumn(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, raw := newValidEnvelope(t)
	env.Transport = "hook"
	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, raw, 1); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	})

	var got string
	if err := s.DB().QueryRow(
		`SELECT transport FROM events WHERE event_id = ?`, env.EventID,
	).Scan(&got); err != nil {
		t.Fatalf("read transport: %v", err)
	}
	if got != "hook" {
		t.Errorf("transport column: got %q, want hook", got)
	}
}

func TestIngestEnvelope_EmptyTransportPersistsAsEmpty(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, raw := newValidEnvelope(t)
	env.Transport = "" // legacy / third-party-bridge envelope
	rawZero, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_ = raw
	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, rawZero, 1); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	})

	var got string
	if err := s.DB().QueryRow(
		`SELECT transport FROM events WHERE event_id = ?`, env.EventID,
	).Scan(&got); err != nil {
		t.Fatalf("read transport: %v", err)
	}
	if got != "" {
		t.Errorf("transport column for empty transport: got %q, want empty", got)
	}
}

func TestIngestEnvelope_PersistsProvenanceColumns(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, _ := newValidEnvelope(t)
	env.SourceAgentVersion = "2.4.1"
	env.Host = "dev-laptop.local"
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, raw, 1); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	})

	var gotVersion, gotHost string
	if err := s.DB().QueryRow(
		`SELECT source_agent_version, host FROM events WHERE event_id = ?`, env.EventID,
	).Scan(&gotVersion, &gotHost); err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	if gotVersion != "2.4.1" {
		t.Errorf("source_agent_version: got %q, want 2.4.1", gotVersion)
	}
	if gotHost != "dev-laptop.local" {
		t.Errorf("host: got %q, want dev-laptop.local", gotHost)
	}
}

func TestIngestEnvelope_DuplicateIsDedupedWithoutTouching(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, raw := newValidEnvelope(t)

	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, raw, 1); err != nil {
			t.Fatalf("first: %v", err)
		}
	})

	var firstCount int
	_ = s.DB().QueryRow(`SELECT event_count FROM sessions`).Scan(&firstCount)

	withTx(t, s, func(tx *sql.Tx) {
		d, err := IngestEnvelope(t.Context(), tx, env, raw, 2)
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
			if _, err := IngestEnvelope(t.Context(), tx, env, raw, int64(1000+i)); err != nil {
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
	env.Tool = &events.Tool{Name: "Bash", CallID: "toolu_abc"}

	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, raw, 1); err != nil {
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
		_, err := IngestEnvelope(t.Context(), tx, nil, nil, 0)
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
			if _, err := IngestEnvelope(t.Context(), tx, env, raw, int64(i)); err != nil {
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

func TestIngestEnvelope_ExtractorsPopulateExtractions(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// A Bash tool_use with a URL in its content_text should produce
	// both a shell_command and a url extraction.
	env, raw := newValidEnvelope(t)
	env.Kind = "tool_use"
	env.Role = "tool"
	env.Tool = &events.Tool{Name: "Bash"}
	env.ContentText = "curl https://example.com/api"
	env.Payload = map[string]any{
		"tool_input": map[string]any{
			"command":     "curl https://example.com/api",
			"description": "hit healthz",
		},
	}

	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, raw, 1); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	})

	rows, err := s.DB().Query(
		`SELECT kind, value, extra_json FROM extractions WHERE event_id=? ORDER BY kind, value`,
		env.EventID,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	type row struct {
		kind, value, extra string
	}
	var got []row
	for rows.Next() {
		var r row
		var ex sql.NullString
		if err := rows.Scan(&r.kind, &r.value, &ex); err != nil {
			t.Fatalf("scan: %v", err)
		}
		r.extra = ex.String
		got = append(got, r)
	}

	if len(got) != 2 {
		t.Fatalf("extractions: got %d rows, want 2: %+v", len(got), got)
	}
	if got[0].kind != "shell_command" || got[0].value != "curl https://example.com/api" {
		t.Errorf("row 0: %+v", got[0])
	}
	if got[0].extra == "" {
		t.Errorf("shell_command should have extra_json from description")
	}
	if got[1].kind != "url" || got[1].value != "https://example.com/api" {
		t.Errorf("row 1: %+v", got[1])
	}
}

func TestIngestEnvelope_NoExtractionsForPlainEvents(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Plain user_prompt with no URLs and no tool → zero extractions.
	env, raw := newValidEnvelope(t)
	env.ContentText = "just a message"
	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, raw, 1); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	})

	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM extractions`).Scan(&n)
	if n != 0 {
		t.Errorf("extractions: got %d, want 0", n)
	}
}

func TestIngestEnvelope_ExtractionsCascadeOnRawDelete(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	env, raw := newValidEnvelope(t)
	env.Tool = &events.Tool{Name: "Bash"}
	env.ContentText = "see https://example.com"
	env.Payload = map[string]any{"tool_input": map[string]any{"command": "ls"}}

	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, raw, 1); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	})

	var before int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM extractions`).Scan(&before)
	if before == 0 {
		t.Fatal("precondition: expected extractions")
	}

	// Deleting the raw envelope should cascade down through events
	// and wipe the associated extractions.
	if _, err := s.DB().Exec(`DELETE FROM raw_envelopes WHERE event_id=?`, env.EventID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var after int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM extractions`).Scan(&after)
	if after != 0 {
		t.Errorf("extractions not cascaded: got %d, want 0", after)
	}
}
