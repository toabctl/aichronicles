## aichronicles search

Full-text search over captured envelopes

### Synopsis

Runs an FTS5 MATCH against events.content_text and prints the
top hits one per line. Query syntax is SQLite FTS5 (phrases in
quotes, AND/OR/NOT, prefix with *).

```
aichronicles search <query> [flags]
```

### Options

```
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help             help for search
      --kind string      filter by event kind (user_prompt, tool_use, …)
      --limit int        max number of hits (default 20)
      --no-dedup         show every row even when the same turn was captured from multiple sources (hook + import)
      --session string   filter by session id or unique prefix
      --since duration   only events within this duration (e.g. 24h, 7d)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
