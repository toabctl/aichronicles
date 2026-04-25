## aichronicles sessions

List sessions in the store, most-recently-ended first

### Synopsis

One tab-separated line per session:

  sess8  started_at  ended_at  event_count  cwd  first_prompt_snippet

Filters stack. Output is grep-friendly; pipe through column -t
for aligned columns.

```
aichronicles sessions [flags]
```

### Options

```
      --agent string     filter by source_agent (e.g. claude-code)
      --cwd string       filter by cwd (exact match)
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help             help for sessions
      --limit int        max sessions to return (default 30)
      --since duration   only sessions whose ended_at is within this duration (search/audit filter on per-event ts_source)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
