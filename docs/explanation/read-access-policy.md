# Read-access policy

Who is allowed to call into `internal/store` directly, and who must go
through the wire (`internal/apiclient` → UDS → `internal/api`)?

## The rule

**In-process readers may share the `*store.Store` handle.
Out-of-process readers MUST go through `internal/apiclient`.**

| Package | Process | Allowed access |
|---|---|---|
| `internal/api` | `aichronicles-api` daemon | direct `internal/store` (it IS the store-owning process) |
| `internal/web` | `aichronicles-api` daemon (folded in) | direct `internal/store` (same handle, same process) |
| `internal/cli` | per-command CLI (`aichronicles ...`) | `internal/apiclient` only |
| `internal/mcp` | `aichronicles mcp serve` (stdio child of the editor) | `internal/apiclient` only |

## Why this shape

The single-writer SQLite invariant the architecture relies on is "one
process at a time writes." The daemon is that process. Any reader
running in a separate process either:

1. Goes through the daemon's UDS so reads + writes serialise on the
   same `*sql.DB` connection pool, OR
2. Opens a second connection against the same SQLite WAL file —
   which works (WAL allows concurrent readers) but means a write
   from one process is visible to the other only after fsync, and
   any out-of-process consumer must be re-validated against schema
   drift.

(2) is the historical pattern; it's brittle and we've migrated away
from it. Today every cross-process consumer reads through the
daemon's HTTP-over-UDS surface and the daemon is the only SQLite
opener.

For same-process consumers (the web HTML surface mounted inside the
daemon process), going through a UDS hop to its own process would
add latency and a connection per page-load for zero benefit. Sharing
the handle is correct.

## Enforcement

`tools/depcheck` enforces this rule mechanically:

- `internal/cli` and `internal/mcp` must not call
  `store.Load*|Save*|Insert*|Update*|Delete*|Has*|Last*|Query*|Vacuum*|Segment*`
  in non-test files. Test files are exempt because they spin up an
  in-process api server against a temp store — sharing the handle in
  a test is fine.
- `internal/web` is INTENTIONALLY not subject to this rule.

Run `go run ./tools/depcheck` to verify; CI runs it on every PR.

## What changes if the unified-daemon-with-folded-web design ever
splits

If `aichronicles web` were to revert to a separately-supervised
sibling process (the architectural follow-up flagged as HIGH #4 in
arch_review_2026_05_13), `internal/web` would also fall under the
"out-of-process" bucket and the depcheck rule would extend to cover
it. Until then, the in-process share is the right choice.

History: arch_review_2026_05_13 MEDIUM #5.
