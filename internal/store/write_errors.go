package store

import (
	"errors"
	"fmt"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrInvalidValue marks a write the store refused because the
// caller's input is invalid: a required field is missing, a value is
// out of range, or the row violates a CHECK / NOT NULL constraint or a
// validation trigger. Handlers answer 400 for it; anything else a
// write returns is a storage fault (500). Match with IsInvalidValue,
// which also recognises the constraint failures SQLite raises itself.
var ErrInvalidValue = errors.New("invalid value")

// invalidf formats a validation error wrapping ErrInvalidValue.
func invalidf(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, ErrInvalidValue)...)
}

// IsInvalidValue reports whether err is a write rejected for invalid
// input — an invalidf error from the store's own checks, or a CHECK,
// NOT NULL or trigger (RAISE(ABORT)) constraint failure.
func IsInvalidValue(err error) bool {
	return errors.Is(err, ErrInvalidValue) || hasSQLiteCode(err,
		sqlite3.SQLITE_CONSTRAINT_CHECK,
		sqlite3.SQLITE_CONSTRAINT_NOTNULL,
		sqlite3.SQLITE_CONSTRAINT_TRIGGER)
}

// IsUnknownReference reports whether err is a write rejected by a
// foreign key: the row names a session, event, LLM output or
// candidate id that does not exist.
func IsUnknownReference(err error) bool {
	return hasSQLiteCode(err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
}

func hasSQLiteCode(err error, codes ...int) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	for _, c := range codes {
		if se.Code() == c {
			return true
		}
	}
	return false
}
