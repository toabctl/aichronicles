package events

// NullString is a domain-pure analogue of sql.NullString — same
// (String, Valid) shape, no database/sql dependency. Lives in
// pkg/events so domain types (events.EventView, SessionDigest, …) can
// model genuinely-nullable text columns without making this package
// depend on a SQL driver.
//
// The Scan method that sql.NullString implements is intentionally
// NOT mirrored here: scanning happens in the store layer, where
// callers convert from sql.NullString → events.NullString at the
// query boundary.
type NullString struct {
	String string
	Valid  bool
}

// OrEmpty returns String when Valid, "" otherwise. Convenience for
// callers that don't care about the null/empty distinction —
// equivalent to the `if n.Valid { use n.String }` pattern that
// shows up at most call sites.
func (n NullString) OrEmpty() string {
	if !n.Valid {
		return ""
	}
	return n.String
}

// NullInt64 mirrors sql.NullInt64 in the same way NullString
// mirrors sql.NullString. Used for genuinely-optional int64 columns
// (timestamps, counts, sequence numbers).
type NullInt64 struct {
	Int64 int64
	Valid bool
}

// OrZero returns Int64 when Valid, 0 otherwise. Convenience for the
// common "treat null as zero" rendering path.
func (n NullInt64) OrZero() int64 {
	if !n.Valid {
		return 0
	}
	return n.Int64
}
