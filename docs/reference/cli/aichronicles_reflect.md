## aichronicles reflect

LLM-derived meta-analysis of recent sessions

### Synopsis

Looks at sessions that ended within --since and asks the LLM,
via the record_reflection tool, to identify recurring task types,
recurring sources of friction, and one workflow change worth
trying. Existing per-session summaries (from `aichronicles
summarize`) are preferred to raw first prompts.

Cached like summarize: same digest list = same prompt_hash =
same cached body. Use --force to re-call. Use --format=json to
emit the raw JSON body instead of the human-readable render.

Requires ANTHROPIC_API_KEY unless the cache hits.

```
aichronicles reflect [flags]
```

### Options

```
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --force            bypass the llm_outputs cache and re-call the LLM
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for reflect
      --limit int        max sessions to feed the LLM, newest first (default 25)
      --model string     LLM model id (default: provider's default)
      --since duration   only consider sessions whose ended_at is within this window (e.g. 24h, 7d) (default 168h0m0s)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
