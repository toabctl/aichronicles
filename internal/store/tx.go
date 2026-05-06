package store

import (
	"context"
	"database/sql"
	"fmt"
)

// WithTx runs fn inside a database transaction, committing when fn
// returns nil and rolling back otherwise. Begin and Commit failures
// are wrapped with %w; the error returned by fn is propagated as-is
// so callers retain the wrapping they chose at the failure site.
//
// The deferred Rollback is a no-op once Commit has succeeded.
func WithTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
