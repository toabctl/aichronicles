## aichronicles sessions

List sessions in the store, most-recently-ended first

### Synopsis

One row per session, columns:

  SESSION  STARTED  ENDED  EVENTS  CWD  FIRST_PROMPT

On a TTY columns are aligned for reading; when piped or
redirected they emit as tab-separated values for awk/cut.
Pass --format=json for a structured payload suitable for jq.

Talks to aichronicles-api over its UDS (override with
--socket or $AICHRONICLES_API_SOCKET).

```
aichronicles sessions [flags]
```

### Options

```
      --cwd string       filter by cwd (exact match)
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for sessions
      --limit int        max sessions to return (default 30)
      --since duration   only sessions whose ended_at is within this duration (e.g. 24h, 7d) (default 0s)
      --socket string    aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
