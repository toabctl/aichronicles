package store

import "strings"

// likeEscaper escapes the LIKE metacharacters so user text matches
// literally. Every pattern built from it must be used with
// `ESCAPE '\'` in the SQL.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// likeContains returns a LIKE pattern (for `… LIKE ? ESCAPE '\'`)
// that matches s as a literal substring. Without the escaping, `_` —
// common in file paths and identifiers — matched any single character
// and `%` any run, so a filter silently matched rows it should not.
func likeContains(s string) string {
	return "%" + likeEscaper.Replace(s) + "%"
}

// likePrefix returns a LIKE pattern (for `… LIKE ? ESCAPE '\'`) that
// matches s as a literal prefix.
func likePrefix(s string) string {
	return likeEscaper.Replace(s) + "%"
}
