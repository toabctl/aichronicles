## aichronicles summaries missing

List sessions in the window that have no cached summary

### Synopsis

Reads the sessions table for entries whose ended_at falls
within --since AND that have no llm_outputs row of
kind='summary'. Useful as the first step before reflect or
propose, both of which now require summaries on every
input session (see commit 9746cef).

Read-only: no LLM calls. Pipe `--format=json | jq -r '.[].id'`
into `aichronicles summarize` for a manual fill, or use
`aichronicles summaries fill` to do it in one shot.

```
aichronicles summaries missing [flags]
```

### Options

```
      --agent string     filter by source_agent (claude-code | codex)
      --cwd string       filter by exact cwd
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for missing
      --limit int        max sessions to list (default 200)
      --since duration   only consider sessions whose ended_at is within this window (e.g. 24h, 7d) (default 3d)
      --socket string    aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
```

### SEE ALSO

* [aichronicles summaries](./aichronicles_summaries.md)	 - Inspect stored LLM outputs (summaries, reflections, proposals)
