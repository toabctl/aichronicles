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
      --agent string                  filter by source agent (claude-code | gemini-cli)
      --db string                     SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --embed-model string            with --semantic: embedding model id to query against (default: text-embedding-3-small)
      --file string                   filter to sessions that touched a file matching this substring
      --format string                 output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help                          help for search
      --kind string                   filter by event kind (user_prompt, tool_use, …)
      --limit int                     max number of hits (default 20)
      --model string                  with --summarize: LLM model id (default: provider's default)
      --no-dedup                      show every row even when the same turn was captured from multiple sources (hook + import)
      --semantic aichronicles embed   vector search via stored embeddings (requires aichronicles embed to have populated event_embeddings; OpenAI key required)
      --session string                filter by session id or unique prefix
      --since duration                only events within this duration (e.g. 24h, 7d) (default 0s)
      --skill string                  filter to sessions that loaded this skill
      --summarize                     synthesise an LLM-written answer from the top hits instead of printing rows (requires ANTHROPIC_API_KEY)
      --tool string                   filter by tool name (e.g. Bash, run_shell_command)
      --top int                       with --summarize: max hits fed to the LLM as grounding context (default 5)
      --with-failures                 filter to sessions that produced at least one tool_failure event
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
