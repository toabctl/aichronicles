## aichronicles sessions

List sessions in the store, most-recently-ended first

### Synopsis

One row per session, columns:

  SESSION  STARTED  ENDED  EVENTS  CWD  FIRST_PROMPT

On a TTY columns are aligned for reading; when piped or
redirected they emit as tab-separated values for awk/cut.
Pass --format=json for a structured payload suitable for jq.
Filters stack.

```
aichronicles sessions [flags]
```

### Options

```
      --agent string     filter by source_agent (e.g. claude-code)
      --cwd string       filter by cwd (exact match)
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for sessions
      --limit int        max sessions to return (default 30)
      --since duration   only sessions whose ended_at is within this duration (search/audit filter on per-event ts_source)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
