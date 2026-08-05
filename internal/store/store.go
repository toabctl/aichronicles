// Package store is the SQLite persistence layer for aichronicles.
//
// Design:
//   - raw_envelopes is append-only and sacred; every other table is
//     derivable from it via re-ingestion.
//   - Schema migrations live in migrations/ and are embedded at build.
//   - WAL mode + foreign keys + busy timeout configured at open time.
//
// The package exposes a thin Store wrapper over *sql.DB. Callers do
// their own SQL — we don't add an ORM layer on top of database/sql.
package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// MigrationsFS exposes the embedded migrations FS so tooling
// (docgen, audit scripts) can iterate the SQL files without
// re-embedding them or shelling out to the filesystem. Read-only.
func MigrationsFS() embed.FS { return migrationsFS }

// EffectiveTsExpr is the canonical SQL expression for "the session's
// effective timestamp" — ended_at_ms when set, otherwise started_at_ms,
// otherwise 0. Used in ORDER BY / WHERE clauses across CLI, MCP, web,
// and the store package itself; pinning the expression here keeps
// the 15-ish call sites in lockstep and matches the expression
// idx_sessions_effective_ts indexes (migration 003).
//
// Splice into raw-string SQL via concatenation:
//
//	q := `SELECT s.id FROM sessions s ORDER BY ` + EffectiveTsExpr + ` DESC`
//
// The column-prefix is included so the expression is unambiguous in
// joined contexts (every consumer aliases the sessions table as `s`).
const EffectiveTsExpr = "COALESCE(s.ended_at_ms, s.started_at_ms, 0)"

// Store is the aichronicles persistence handle. Construct with Open
// (consumers) or OpenMigrate (the api daemon and admin tools); call
// Close on shutdown. Safe for concurrent use (*sql.DB is; triggers
// and transactions handle write ordering).
type Store struct {
	db *sql.DB
}

// ErrSchemaTooOld is returned by Open when the DB's schema_version is
// behind the build's embedded migrations. The migrator (api daemon, or
// `aichronicles migrate` if/when added) must run before consumers can
// open the DB.
var ErrSchemaTooOld = errors.New("store: schema_version older than this build's expected version")

// ErrSchemaTooNew is returned by Open when the DB's schema_version is
// AHEAD of the build's embedded migrations — a downgrade scenario.
// Refusing to open prevents an older binary from clobbering a newer
// schema's data.
var ErrSchemaTooNew = errors.New("store: schema_version newer than this build's expected version")

// latestSchemaVersion is the highest migration version embedded in
// migrationsFS. Computed once on first call. Used by Open to verify
// schema parity and by OpenMigrate as the target version.
var latestSchemaVersion = sync.OnceValue(func() int {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		// embed.FS is build-time — read failure means a corrupted
		// binary, not a runtime condition. Panic so the daemon
		// fails to start instead of running with a phantom schema.
		panic("store: read embedded migrations dir: " + err.Error())
	}
	max := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := parseMigrationVersion(e.Name())
		if err != nil {
			panic("store: parse embedded migration filename " + e.Name() + ": " + err.Error())
		}
		if v > max {
			max = v
		}
	}
	return max
})

// LatestSchemaVersion returns the highest migration version embedded
// in this build. Surfaced so callers (the daemon, an admin tool) can
// log "running against schema vN" without re-walking the FS.
func LatestSchemaVersion() int { return latestSchemaVersion() }

// Open returns a Store backed by the SQLite file at path WITHOUT
// running migrations, and verifies the DB's schema_version equals
// the build's LatestSchemaVersion. The file and its parent directories
// are created if missing.
//
// Consumer processes (web UI, CLI subcommands, MCP server) call Open.
// Only the api daemon — the sole writer and migrator — calls
// OpenMigrate. This separation prevents two processes from racing
// on the migration sequence and surfaces version drift loudly
// instead of papering over it: if a consumer's binary is newer
// than the daemon's, Open returns ErrSchemaTooOld; if older,
// ErrSchemaTooNew.
//
// On a never-initialised DB (schema_version=0, no meta table)
// Open returns ErrSchemaTooOld so the caller knows to start the
// daemon first.
//
// Pragmas applied to every pooled connection via the DSN:
//   - journal_mode=WAL (concurrent readers during writes)
//   - foreign_keys=ON  (cascade deletes work; referential integrity)
//   - busy_timeout=5000 (absorb short contention without ERR)
//   - synchronous=NORMAL (durable enough with WAL, faster than FULL)
//
// Pragmas in the DSN apply at connection open, so every connection
// in the pool gets them — vs `db.Exec("PRAGMA …")` which only
// affects whichever single connection happened to handle that
// statement. busy_timeout in particular MUST be set per-connection
// for the pool to avoid SQLITE_BUSY under concurrent writers.
func Open(path string) (*Store, error) {
	s, err := openWithoutMigrate(path)
	if err != nil {
		return nil, err
	}
	current, err := s.currentSchemaVersion()
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	expected := latestSchemaVersion()
	switch {
	case current < expected:
		_ = s.Close()
		return nil, fmt.Errorf("%w: db=%d build=%d", ErrSchemaTooOld, current, expected)
	case current > expected:
		_ = s.Close()
		return nil, fmt.Errorf("%w: db=%d build=%d", ErrSchemaTooNew, current, expected)
	}
	return s, nil
}

// OpenMigrate opens a Store and applies any pending migrations to
// bring the DB up to LatestSchemaVersion. Reserved for the api daemon
// (cmd/aichronicles-api) and tests; every other production caller
// should use Open to surface schema drift instead of silently
// migrating concurrently.
func OpenMigrate(path string) (*Store, error) {
	s, err := openWithoutMigrate(path)
	if err != nil {
		return nil, err
	}
	if err := s.migrate(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// openWithoutMigrate is the shared body of Open and OpenMigrate:
// resolve the DSN, configure pool sizing, return the wrapped Store.
// No migration, no schema check.
func openWithoutMigrate(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		// FAST zeroes freed content within pages SQLite is already
		// rewriting, so deleted rows stop lingering verbatim in the
		// file. This matters here specifically because ingest_pending
		// holds raw POST bodies pre-redaction, and Scrub rewrites
		// rows that held secrets — without it, both leave the
		// original bytes recoverable in free pages until a manual
		// VACUUM, which on a multi-GB store needs 2x free space and
		// so rarely happens.
		//
		// FAST rather than ON deliberately: ON also walks free pages
		// that are not otherwise being touched, which is real write
		// amplification on an append-heavy ingest path. FAST is the
		// cheap 90% and costs essentially nothing.
		"&_pragma=secure_delete(FAST)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// modernc.org/sqlite handles concurrent connections fine; a single
	// *sql.DB can multiplex. We keep MaxOpenConns modest because SQLite
	// serializes writes internally — more connections = more waiting.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	return &Store{db: db}, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// SetMaxOpenConns overrides the connection-pool cap that Open
// configured (default 4). Non-positive values are ignored. Designed
// for the daemon to forward [limits].sqlite_max_open_conns from the
// config file without exposing the *sql.DB. SetMaxIdleConns is
// proportionally scaled to half the cap (min 1) so the pool can
// actually keep more than the previous default warm.
func (s *Store) SetMaxOpenConns(n int) {
	if n <= 0 {
		return
	}
	s.db.SetMaxOpenConns(n)
	idle := n / 2
	if idle < 1 {
		idle = 1
	}
	s.db.SetMaxIdleConns(idle)
}

// DB returns the underlying *sql.DB. Exposed so callers (daemon
// handlers, CLI subcommands, tests) can run their own SQL — the
// package deliberately does not wrap every query in a typed method.
func (s *Store) DB() *sql.DB {
	return s.db
}

// inPlaceholders prepares the (?,?,...) placeholder string and the
// []any argument slice for a SQL `IN` clause. Returns ("", nil)
// when ids is empty so the caller can short-circuit to a no-query
// result.
//
// Centralised so the `strings.Repeat(",?", N)[1:]` micro-trick lives
// in one place rather than three.
func inPlaceholders(ids []string) (string, []any) {
	if len(ids) == 0 {
		return "", nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.Repeat(",?", len(ids))[1:], args
}
