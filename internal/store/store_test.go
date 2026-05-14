package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// openTemp creates a Store at a fresh temp path, failing the test on
// any error, and ensures cleanup. Uses OpenMigrate so tests get a
// fully-migrated DB without needing a daemon — Open by contract
// returns ErrSchemaTooOld on a fresh file.
func openTemp(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := OpenMigrate(path)
	if err != nil {
		t.Fatalf("OpenMigrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// closeStore is a defer-friendly wrapper for tests that want to close
// a Store explicitly (e.g. to reopen under a different handle).
func closeStore(s *Store) { _ = s.Close() }

func TestOpen_FreshCreatesSchema(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// meta table seeded with the latest schema version the migration
	// runner knows about. Bump here when you add a new migration so
	// this test catches accidental drops.
	var v string
	if err := s.DB().QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v != "28" {
		t.Errorf("schema_version: got %q, want 28", v)
	}

	// Expected tables all exist. proposed_skills was renamed to
	// skill_candidates in migration 021 (AutoSkill vocabulary
	// alignment) — keep this list in sync with the live schema.
	for _, name := range []string{"meta", "raw_envelopes", "sessions", "events", "events_fts", "events_fts_trigram", "extractions", "extractions_fts", "llm_outputs", "session_links", "session_outcomes", "skill_candidates", "semantic_facts", "ingest_pending"} {
		var got string
		err := s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE name=?`, name).Scan(&got)
		if err != nil {
			t.Errorf("missing object %s: %v", name, err)
		}
	}
}

func TestOpen_ReopenIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.db")

	s1, err := OpenMigrate(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	// Insert a sentinel row to prove data survives migration re-run.
	_, err = s1.DB().Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"evt-1", 1, "claude-code", "sess-a", 1, 2, "{}",
	)
	if err != nil {
		t.Fatalf("insert sentinel: %v", err)
	}
	_ = s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer closeStore(s2)

	var count int
	if err := s2.DB().QueryRow(`SELECT COUNT(*) FROM raw_envelopes WHERE event_id='evt-1'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("sentinel lost: got %d, want 1", count)
	}
}

func TestPragmas_WALAndForeignKeys(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	var mode string
	if err := s.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode: got %q, want wal", mode)
	}

	var fk int
	if err := s.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys: got %d, want 1", fk)
	}
}

func TestOpen_BadPathReturnsError(t *testing.T) {
	t.Parallel()
	// A path through a non-existent directory with a bad segment.
	// modernc.org/sqlite only errors at first I/O, so force a query.
	_, err := Open("/proc/self/attr/current/nope.db")
	if err == nil {
		t.Fatal("expected error for unusable path")
	}
}

// TestOpen_FreshDBReturnsSchemaTooOld verifies that the consumer-side
// Open refuses to operate on a never-initialised DB rather than
// silently creating tables. The migrator (api daemon or
// OpenMigrate-in-tests) is the only way to bootstrap.
func TestOpen_FreshDBReturnsSchemaTooOld(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh.db")
	_, err := Open(path)
	if !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("Open(fresh-db): got %v, want ErrSchemaTooOld", err)
	}
}

// TestOpen_SchemaTooNew verifies that opening a DB whose
// schema_version is AHEAD of the build's expected version surfaces
// ErrSchemaTooNew — the downgrade guard.
func TestOpen_SchemaTooNew(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "future.db")
	// Migrate normally, then manually bump schema_version past the
	// build's expected latest to simulate a forward-evolved DB.
	s1, err := OpenMigrate(path)
	if err != nil {
		t.Fatalf("OpenMigrate seed: %v", err)
	}
	future := LatestSchemaVersion() + 1
	if _, err := s1.DB().Exec(
		`UPDATE meta SET value=? WHERE key='schema_version'`,
		strconv.Itoa(future),
	); err != nil {
		t.Fatalf("bump schema_version: %v", err)
	}
	_ = s1.Close()

	_, err = Open(path)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Open(future-db): got %v, want ErrSchemaTooNew", err)
	}
}

// insertRawAndEvent is a helper used by trigger-focused tests.
func insertRawAndEvent(t *testing.T, s *Store, eventID, sessionID, sourceSessionID, kind, contentText string, ts int64) {
	t.Helper()
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES (?, (SELECT COALESCE(MAX(ingest_seq),0)+1 FROM raw_envelopes), ?, ?, ?, ?, ?)`,
		eventID, "claude-code", sourceSessionID, ts, ts+1, "{}",
	)
	if err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	_, err = tx.Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		sessionID, "claude-code", sourceSessionID,
	)
	if err != nil {
		t.Fatalf("session upsert: %v", err)
	}
	_, err = tx.Exec(
		`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, cwd, content_text) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		eventID, sessionID, "claude-code", kind, ts, "/tmp/proj", contentText,
	)
	if err != nil {
		t.Fatalf("event insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestTrigger_SessionAggregatesUpdateOnInsert(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	insertRawAndEvent(t, s, "e1", "sess-id", "ext-1", "user_prompt", "hello world", 100)
	insertRawAndEvent(t, s, "e2", "sess-id", "ext-1", "assistant_message", "hi", 200)
	insertRawAndEvent(t, s, "e3", "sess-id", "ext-1", "tool_use", "bash", 150) // out-of-order ts

	var cnt int
	var startMs, endMs int64
	var cwd sql.NullString
	err := s.DB().QueryRow(
		`SELECT event_count, started_at_ms, ended_at_ms, cwd FROM sessions WHERE id='sess-id'`,
	).Scan(&cnt, &startMs, &endMs, &cwd)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}

	if cnt != 3 {
		t.Errorf("event_count: got %d, want 3", cnt)
	}
	if startMs != 100 {
		t.Errorf("started_at_ms: got %d, want 100 (min across inserts)", startMs)
	}
	if endMs != 200 {
		t.Errorf("ended_at_ms: got %d, want 200 (max across inserts)", endMs)
	}
	if !cwd.Valid || cwd.String != "/tmp/proj" {
		t.Errorf("cwd: got %v", cwd)
	}
}

func TestTrigger_FTSIndexPopulates(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	insertRawAndEvent(t, s, "e1", "sess-a", "s", "user_prompt", "the quick brown fox", 1)
	insertRawAndEvent(t, s, "e2", "sess-a", "s", "user_prompt", "lazy dog jumps", 2)

	rows, err := s.DB().Query(
		`SELECT e.event_id FROM events_fts f JOIN events e ON e.rowid = f.rowid
		 WHERE events_fts MATCH ? ORDER BY rank`,
		"fox",
	)
	if err != nil {
		t.Fatalf("fts query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		hits = append(hits, id)
	}
	if len(hits) != 1 || hits[0] != "e1" {
		t.Errorf("fox search: got %v, want [e1]", hits)
	}
}

func TestTrigger_FTSUpdatesOnEventUpdate(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	insertRawAndEvent(t, s, "e1", "sess-a", "s", "user_prompt", "cats", 1)

	if _, err := s.DB().Exec(`UPDATE events SET content_text='dogs' WHERE event_id='e1'`); err != nil {
		t.Fatalf("update: %v", err)
	}

	// 'cats' should no longer match
	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM events_fts WHERE events_fts MATCH ?`, "cats",
	).Scan(&n); err != nil {
		t.Fatalf("fts count cats: %v", err)
	}
	if n != 0 {
		t.Errorf("FTS still has 'cats' after update: %d", n)
	}
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM events_fts WHERE events_fts MATCH ?`, "dogs",
	).Scan(&n); err != nil {
		t.Fatalf("fts count dogs: %v", err)
	}
	if n != 1 {
		t.Errorf("FTS missing 'dogs' after update: %d", n)
	}
}

func TestTrigger_FTSAndCascadesOnDeleteEvent(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	insertRawAndEvent(t, s, "e1", "sess-a", "s", "user_prompt", "findable", 1)

	// Attach an extraction.
	_, err := s.DB().Exec(
		`INSERT INTO extractions(event_id, session_id, kind, value) VALUES (?, ?, ?, ?)`,
		"e1", "sess-a", "url", "https://example.com",
	)
	if err != nil {
		t.Fatalf("extraction insert: %v", err)
	}

	if _, err := s.DB().Exec(`DELETE FROM events WHERE event_id='e1'`); err != nil {
		t.Fatalf("delete event: %v", err)
	}

	var extCount, ftsCount int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM extractions WHERE event_id='e1'`).Scan(&extCount)
	if extCount != 0 {
		t.Errorf("extraction should cascade: got %d", extCount)
	}
	_ = s.DB().QueryRow(
		`SELECT COUNT(*) FROM events_fts WHERE events_fts MATCH ?`, "findable",
	).Scan(&ftsCount)
	if ftsCount != 0 {
		t.Errorf("FTS should drop row on event delete: got %d", ftsCount)
	}
}

func TestCascades_DeleteRawPropagatesToEvents(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	insertRawAndEvent(t, s, "e1", "sess-a", "s", "user_prompt", "x", 1)

	if _, err := s.DB().Exec(`DELETE FROM raw_envelopes WHERE event_id='e1'`); err != nil {
		t.Fatalf("delete raw: %v", err)
	}
	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE event_id='e1'`).Scan(&n)
	if n != 0 {
		t.Errorf("event should cascade-delete when raw goes: got %d", n)
	}
}

func TestUniqueConstraints(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	_, err := s.DB().Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES ('e1', 1, 'a', 's', 0, 0, '{}')`,
	)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Duplicate event_id → fail
	_, err = s.DB().Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES ('e1', 2, 'a', 's', 0, 0, '{}')`,
	)
	if err == nil {
		t.Error("expected PRIMARY KEY violation on duplicate event_id")
	}

	// Duplicate ingest_seq → fail
	_, err = s.DB().Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES ('e2', 1, 'a', 's', 0, 0, '{}')`,
	)
	if err == nil {
		t.Error("expected UNIQUE violation on duplicate ingest_seq")
	}

	// Duplicate (source_agent, source_session_id) on sessions → fail
	_, err = s.DB().Exec(`INSERT INTO sessions(id, source_agent, source_session_id) VALUES ('id1', 'a', 's')`)
	if err != nil {
		t.Fatalf("session insert: %v", err)
	}
	_, err = s.DB().Exec(`INSERT INTO sessions(id, source_agent, source_session_id) VALUES ('id2', 'a', 's')`)
	if err == nil {
		t.Error("expected UNIQUE violation on duplicate (source_agent, source_session_id)")
	}
}

// TestWAL_ConcurrentReadersDuringWrite proves the WAL-mode promise.
func TestWAL_ConcurrentReadersDuringWrite(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Seed a row so readers have something to fetch.
	insertRawAndEvent(t, s, "seed", "sess-a", "s", "user_prompt", "first", 1)

	// Start a writer in a transaction and hold it open.
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	// Make the writer dirty so WAL actually matters here.
	if _, err := tx.Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES ('w1', 100, 'a', 's', 0, 0, '{}')`,
	); err != nil {
		t.Fatalf("write inside tx: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var n int
			// Readers should see the seeded row (pre-tx snapshot) without blocking.
			if err := s.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("concurrent reader failed: %v", err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestParseMigrationVersion exercises the filename→version helper.
func TestParseMigrationVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		want    int
		wantErr bool
	}{
		{"001_initial.sql", 1, false},
		{"002_add_embeddings.sql", 2, false},
		{"10_bad.sql", 10, false},
		{"not_a_version.sql", 0, true},
		{"initial.sql", 0, true},
		{"_.sql", 0, true},
	}
	for _, tc := range cases {
		got, err := parseMigrationVersion(tc.name)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: want error, got none", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

// TestSetMaxOpenConns_Override proves the daemon can replace the
// default pool cap from a config-driven value, and that non-positive
// inputs leave the existing setting untouched.
func TestSetMaxOpenConns_Override(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Default applied by Open is 4. Bumping should take effect.
	s.SetMaxOpenConns(8)
	if got := s.DB().Stats().MaxOpenConnections; got != 8 {
		t.Errorf("after SetMaxOpenConns(8): MaxOpenConnections=%d, want 8", got)
	}

	// Non-positive must be a no-op so a zero-valued config never wipes
	// out the open-time default.
	s.SetMaxOpenConns(0)
	if got := s.DB().Stats().MaxOpenConnections; got != 8 {
		t.Errorf("after SetMaxOpenConns(0): MaxOpenConnections=%d, want unchanged 8", got)
	}
	s.SetMaxOpenConns(-3)
	if got := s.DB().Stats().MaxOpenConnections; got != 8 {
		t.Errorf("after SetMaxOpenConns(-3): MaxOpenConnections=%d, want unchanged 8", got)
	}
}
