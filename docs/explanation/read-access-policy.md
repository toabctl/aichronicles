# Read-access policy

Who is allowed to call into `internal/store` directly, and who must go
through the wire (`internal/apiclient` → UDS → `internal/api`)?

## The rule

**Only `aichronicles-api` is allowed to bind `internal/store`. Every
other process — CLI, web, MCP — reads + writes through
`internal/apiclient` against the daemon's UDS.**

| Package | Process | Allowed access |
|---|---|---|
| `internal/api` | `aichronicles-api` daemon | direct `internal/store` (it IS the store-owning process) |
| `internal/web` | `aichronicles-web.service` daemon (separate process from api) | `internal/apiclient` only |
| `internal/cli` | per-command CLI (`aichronicles ...`) | `internal/apiclient` for read paths; direct `internal/store` only for designated writer commands (induction, reflect, scrub, prune, backfill) that legitimately need exclusive store access |
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

The previous version of this doc described an alternate design where
`internal/web` ran inside the api daemon process and shared the
`*store.Store` handle in-process. That design was reverted —
`aichronicles-web` is now a separately-supervised systemd unit
(`aichronicles-web.service`) that fronts the api's UDS. The
blast-radius isolation (a web-handler bug can't take down ingest) was
the load-bearing reason; the latency hit of the extra hop is
negligible for the personal-use scale.

## Enforcement

`tools/depcheck` enforces this rule mechanically:

- `internal/cli`, `internal/mcp`, and `internal/web` must not call
  `store.Load*|Save*|Insert*|Update*|Delete*|Has*|Last*|Query*|Vacuum*|Segment*`
  in non-test files. Test files are exempt because they spin up an
  in-process api server against a temp store — sharing the handle in
  a test is fine.
- Several supplementary layering rules guard the dependency arrows:
  `internal/store` must not import `internal/api`, `internal/apiclient`,
  the orchestration layers, or `net/http`; `internal/redact` must not
  import any sibling `internal/*`; `cmd/aichronicles` must not import
  `internal/api`. Run `go run ./tools/depcheck` to verify the full set;
  CI runs it on every PR.

History: arch_review_2026_05_13 MEDIUM #5 (cross-process migration),
arch_review_2026_05_14 (web split + depcheck rule expansion),
arch_review_2026_05_14_late (post-split doc refresh).
