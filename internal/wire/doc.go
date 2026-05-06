// Package api defines the wire types served by aichronicles-api
// over HTTP+JSON on its Unix-domain socket. It is the public
// schema layer that decouples consumers (CLI, MCP server, web UI,
// third-party clients) from the SQLite-flavored shapes in
// internal/store.
//
// Stability promise: pkg/api is the contract. Field names, JSON
// tags, enum values, and error shapes here are what callers
// depend on. A change to internal/store row types must not
// require a change to pkg/api unless we are deliberately evolving
// the wire.
//
// Design rules enforced by review (and in CI as we grow):
//
//   - No database/sql imports. sql.NullString / sql.NullInt64 are
//     not JSON-clean; use *string / *int64 (or the
//     pkg/events.NullString helper) instead.
//   - No net/http imports. These types are transport-agnostic; the
//     handler in internal/api is the only place that touches HTTP.
//   - No internal/* imports. internal/api translates between
//     internal/store and pkg/api; this package never reaches in
//     the other direction.
//   - JSON tags on every exported field. Validate round-trips with
//     a test fixture per type.
//
// Errors on the wire follow RFC 7807 problem+json (see Problem).
// Pagination is opaque-cursor based (see Cursor).
package wire
