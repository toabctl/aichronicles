package cli

import (
	"database/sql"
	"time"

	"github.com/toabctl/aichronicles/internal/nullable"
)

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
