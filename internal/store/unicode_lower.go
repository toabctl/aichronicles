package store

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

// unicodeLowerFunc is the SQL name of the Unicode-aware lowercase
// function every Store connection gets. SQLite's built-in lower() and
// LIKE's case-insensitivity only fold ASCII (the driver is built
// without ICU), so a case-insensitive match written with them silently
// fails for any non-ASCII text: `lower('Über')` is 'Über', and an
// episode about "Überarbeite die Doku" was unreachable by "über".
const unicodeLowerFunc = "unicode_lower"

// registerUnicodeLower registers unicodeLowerFunc with the driver
// exactly once per process. Registration is driver-global and only
// affects connections opened afterwards, so openWithoutMigrate calls
// it before sql.Open.
var registerUnicodeLower = sync.OnceValue(func() error {
	return sqlite.RegisterDeterministicScalarFunction(unicodeLowerFunc, 1, unicodeLower)
})

// unicodeLower implements unicodeLowerFunc: strings.ToLower over TEXT
// (and BLOB, read as UTF-8), NULL for NULL, other types unchanged —
// the same shape as the built-in lower().
func unicodeLower(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	switch v := args[0].(type) {
	case nil:
		return nil, nil
	case string:
		return strings.ToLower(v), nil
	case []byte:
		return strings.ToLower(string(v)), nil
	case int64, float64:
		return v, nil
	default:
		return nil, fmt.Errorf("%s: unsupported argument type %T", unicodeLowerFunc, v)
	}
}
