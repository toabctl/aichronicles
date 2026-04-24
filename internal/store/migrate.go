package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// migrate applies every embedded migration whose numeric prefix is
// greater than the current meta.schema_version. Migrations are named
// NNN_description.sql (zero-padded). Within a migration, the SQL runs
// inside a single transaction so partial application can't leave us
// half-migrated.
func (s *Store) migrate() error {
	current, err := s.currentSchemaVersion()
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	type migration struct {
		version int
		name    string
	}
	var ms []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := parseMigrationVersion(e.Name())
		if err != nil {
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		ms = append(ms, migration{version: v, name: e.Name()})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })

	for _, m := range ms {
		if m.version <= current {
			continue
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+m.name)
		if err != nil {
			return fmt.Errorf("read %s: %w", m.name, err)
		}
		if err := s.applyMigration(m.name, body); err != nil {
			return err
		}
	}
	return nil
}

// currentSchemaVersion returns meta.schema_version if present, 0 if
// the meta table doesn't exist yet (fresh DB).
func (s *Store) currentSchemaVersion() (int, error) {
	var exists int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='meta'`,
	).Scan(&exists)
	if err != nil {
		return 0, fmt.Errorf("check meta table: %w", err)
	}
	if exists == 0 {
		return 0, nil
	}
	var v string
	err = s.db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("parse schema_version %q: %w", v, err)
	}
	return n, nil
}

// applyMigration runs one SQL file in a transaction. SQLite supports
// DDL inside transactions (unlike some other engines) so a failed
// migration rolls back cleanly.
func (s *Store) applyMigration(name string, body []byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx for %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(string(body)); err != nil {
		return fmt.Errorf("apply %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", name, err)
	}
	return nil
}

// parseMigrationVersion extracts the leading integer from a migration
// filename like "001_initial.sql" → 1. An underscore must follow.
func parseMigrationVersion(name string) (int, error) {
	idx := strings.Index(name, "_")
	if idx <= 0 {
		return 0, fmt.Errorf("missing version prefix (expected NNN_...sql)")
	}
	v, err := strconv.Atoi(name[:idx])
	if err != nil {
		return 0, fmt.Errorf("bad version prefix %q: %w", name[:idx], err)
	}
	return v, nil
}
