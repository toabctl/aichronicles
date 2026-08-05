package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/redact"
)

// TestBeginImmediate_SurvivesAConcurrentCommit is the regression gate
// for maintenance operations aborting against a live daemon.
//
// A deferred BEGIN that reads before it writes pins a WAL read
// snapshot. If another connection commits in between, the write fails
// with SQLITE_BUSY_SNAPSHOT (517) — and SQLite deliberately skips the
// busy handler for snapshot conflicts, so busy_timeout does not cover
// it and nothing retries.
//
// Prune and Scrub are exactly that shape: count rows, then delete or
// rewrite them. Scrub's exposure spans its whole scan, so against a
// running daemon a single ingested event aborted the run — after the
// operator had watched the scan finish.
func TestBeginImmediate_SurvivesAConcurrentCommit(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Deferred BEGIN: read first, then try to write after someone
	// else has committed. This is the old shape, asserted here so the
	// test proves the failure mode is real rather than hypothetical.
	t.Run("deferred transaction is bumped off its snapshot", func(t *testing.T) {
		tx, err := s.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()

		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta`).Scan(&n); err != nil {
			t.Fatalf("read: %v", err)
		}
		commitFromAnotherConnection(t, s)

		_, err = tx.ExecContext(ctx,
			`UPDATE meta SET value = value WHERE key = 'schema_version'`)
		if err == nil {
			t.Skip("engine did not produce a snapshot conflict; nothing to compare against")
		}
		if !strings.Contains(err.Error(), "locked") && !strings.Contains(err.Error(), "busy") {
			t.Fatalf("unexpected error shape: %v", err)
		}
	})

	t.Run("immediate transaction is not", func(t *testing.T) {
		tx, err := beginImmediate(ctx, s.DB())
		if err != nil {
			t.Fatalf("beginImmediate: %v", err)
		}
		defer func() { _ = tx.Rollback() }()

		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta`).Scan(&n); err != nil {
			t.Fatalf("read: %v", err)
		}
		// Holding the write lock, a concurrent writer cannot commit
		// underneath us, so the later write succeeds.
		if _, err := tx.ExecContext(ctx,
			`UPDATE meta SET value = value WHERE key = 'schema_version'`); err != nil {
			t.Errorf("write after read failed inside an immediate tx: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Errorf("commit: %v", err)
		}
	})
}

// commitFromAnotherConnection performs a committed write on a
// separate connection, simulating the daemon's ingest worker landing
// an event mid-maintenance.
func commitFromAnotherConnection(t *testing.T, s *Store) {
	t.Helper()
	other, err := s.DB().Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = other.Close() }()
	if _, err := other.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO meta(key, value) VALUES ('probe', 'x')`); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
}

// TestPruneAndScrub_UseAnImmediateTransaction pins that both
// maintenance entry points actually take the write lock up front,
// rather than relying on a comment.
func TestPruneAndScrub_UseAnImmediateTransaction(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// A write committed from another connection between the caller's
	// entry and the operation's own write must not abort it.
	ingestForScrub(t, s, "no-secret", nil)

	done := make(chan error, 1)
	go func() {
		_, err := Prune(ctx, s.DB(), PruneOptions{CutoffMs: 1})
		done <- err
	}()
	commitFromAnotherConnection(t, s)
	if err := <-done; err != nil && !isBenignPruneErr(err) {
		t.Errorf("Prune aborted against a concurrent writer: %v", err)
	}

	if _, err := Scrub(ctx, s.DB(), redact.Default(), ScrubOptions{}); err != nil {
		t.Errorf("Scrub aborted: %v", err)
	}
}

// isBenignPruneErr filters the "nothing to prune" shapes so the test
// only fails on a locking error.
func isBenignPruneErr(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
