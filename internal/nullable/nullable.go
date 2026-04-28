// Package nullable centralises the small "extract from sql.Null* /
// fall back to default" helpers that the CLI, web, and MCP surfaces
// each rewrote independently.
//
// The helpers come in two flavours:
//
//   - String-rendering: turn a sql.Null* into a display string,
//     falling back to "-" when the column is NULL or zero. Used by
//     the table/text outputs that need every cell populated.
//
//   - JSON-friendly pointer-rendering: turn a sql.Null* into a *T
//     so the JSON output renders `null` instead of `0` or "" for
//     missing values. Used by --format=json paths.
//
// Write-side wrappers (string → sql.NullString) stay in store/ingest.go
// where the only producer lives; this package is read-side only.
package nullable

import "database/sql"

// OrDash returns the string content of a sql.NullString, or "-"
// when NULL or empty. The "-" sentinel is the convention every
// table/text output in the codebase already uses, so a uniform
// helper keeps the visual look consistent across surfaces.
func OrDash(n sql.NullString) string {
	if !n.Valid || n.String == "" {
		return "-"
	}
	return n.String
}

// Int64Ptr lifts sql.NullInt64 into *int64 so JSON output renders
// `null` rather than `0` for missing timestamps. Always returns a
// fresh pointer (never aliases shared state).
func Int64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// StringPtr lifts sql.NullString into *string. Empty-but-valid is
// preserved as "" (distinct from null) — the schema allows empty
// columns to be legitimately empty rather than absent.
func StringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}
