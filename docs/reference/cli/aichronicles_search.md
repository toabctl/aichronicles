## aichronicles search

Full-text search over captured envelopes

### Synopsis

Searches across captured envelopes and prints the top hits
one per line. Type plain words; bare tokens match by prefix
(`mongo` finds `mongodb`). Wrap exact matches in double
quotes (`"panic stack"`). Identifiers and paths can be
typed verbatim (`migrate.go`). Pass --format=json for a
structured payload suitable for jq.

```
aichronicles search <query> [flags]
```

### Options

```
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for search
      --kind string      filter by event kind (user_prompt, tool_use, …)
      --limit int        max number of hits (default 20)
      --model string     with --summarize: LLM model id (default: provider's default)
      --no-dedup         show every row even when the same turn was captured from multiple sources (hook + import)
      --session string   filter by session id or unique prefix
      --since duration   only events within this duration (e.g. 24h, 7d) (default 0s)
      --summarize        synthesise an LLM-written answer from the top hits instead of printing rows (requires ANTHROPIC_API_KEY)
      --top int          with --summarize: max hits fed to the LLM as grounding context (default 5)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
