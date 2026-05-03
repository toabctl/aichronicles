package cli

import (
	"database/sql"
	"time"

	"github.com/toabctl/aichronicles/internal/nullable"
)

// nullableInt64 converts a sql.NullInt64 into the *int64 form used
// by the CLI's JSON-mode renderers. Shared across audit, summaries,
// and any other read CLI that still pulls rows directly out of the
// store. The /v1/* JSON wire types use *T pointers natively, so
// callers going through apiclient should not need this.
func nullableInt64(n sql.NullInt64) *int64 { return nullable.Int64Ptr(n) }

// formatTsNullable renders a sql.NullInt64 epoch-millis as the
// canonical human-readable form, or "-" when invalid.
func formatTsNullable(n sql.NullInt64) string {
	if !n.Valid {
		return "-"
	}
	return formatTimeForUser(n.Int64, time.Now())
}

// nullStringOrDash renders a sql.NullString as its value or "-".
// Thin wrapper kept so the call sites read like a column renderer
// rather than reaching into internal/nullable directly.
func nullStringOrDash(s sql.NullString) string { return nullable.OrDash(s) }
