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
// Pragmas applied once at open:
//   - journal_mode=WAL (concurrent readers during writes)
//   - foreign_keys=ON  (cascade deletes work; referential integrity)
//   - busy_timeout=5000 (absorb short contention without ERR)
//   - synchronous=NORMAL (durable enough with WAL, faster than FULL)
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// modernc.org/sqlite handles concurrent connections fine; a single
	// *sql.DB can multiplex. We keep MaxOpenConns modest because SQLite
	// serializes writes internally — more connections = more waiting.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA synchronous = NORMAL;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("exec %q: %w", p, err)
		}
	}

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
