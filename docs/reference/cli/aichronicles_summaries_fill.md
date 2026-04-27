## aichronicles summaries fill

Summarize every session in the window that has no cached summary

### Synopsis

Iterates the missing-summary list (see `summaries missing`)
and calls summarize on each entry. Sequential: one LLM call
at a time. Per-session failures (rate limits, malformed
sessions) are reported and skipped — the batch continues.
Ctrl-C stops cleanly after the in-flight session commits.

Idempotent: re-running on the same window does nothing once
every session has a summary. The default --limit=100 caps a
runaway fill on a wide window; loosen as needed.

Requires ANTHROPIC_API_KEY (or the configured api_key_command).

```
aichronicles summaries fill [flags]
```

### Options

```
      --agent string     filter by source_agent (claude-code | codex)
      --cwd string       filter by exact cwd
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for fill
      --limit int        max sessions to summarize in this run (default 100)
      --model string     LLM model id (default: provider's default)
      --since duration   only consider sessions whose ended_at is within this window (e.g. 24h, 7d) (default 168h0m0s)
```

### SEE ALSO

* [aichronicles summaries](./aichronicles_summaries.md)	 - Inspect stored LLM outputs (summaries, reflections, proposals)
