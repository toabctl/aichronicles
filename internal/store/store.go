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
	"fmt"

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

// Store is the aichronicles persistence handle. Construct with Open;
// call Close on shutdown. Safe for concurrent use (*sql.DB is; triggers
// and transactions handle write ordering).
type Store struct {
	db *sql.DB
}

// Open returns a Store backed by the SQLite file at path. The file and
// its parent directories are created if missing. Migrations are applied
// idempotently so calling Open on an existing DB is safe.
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
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// modernc.org/sqlite handles concurrent connections fine; a single
	// *sql.DB can multiplex. We keep MaxOpenConns modest because SQLite
	// serializes writes internally — more connections = more waiting.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
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
