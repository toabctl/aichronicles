## aichronicles propose

LLM-suggested skills / CLAUDE.md entries / scripts from recent sessions

### Synopsis

Reads recent sessions (same window semantics as `reflect`) and,
via the record_proposal tool, asks the LLM to propose concrete
reusable capabilities: new slash-commands, CLAUDE.md rules, and
scripts to pre-build. The system prompt forbids generic advice —
every suggestion must cite at least one session as evidence.

Cached on prompt_hash in llm_outputs with kind=propose. Use
--force to re-call. Use --format=json to emit the raw JSON body.

Requires ANTHROPIC_API_KEY unless the cache hits.

```
aichronicles propose [flags]
```

### Options

```
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --force            bypass the llm_outputs cache and re-call the LLM
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for propose
      --limit int        max sessions to feed the LLM, newest first (default 25)
      --model string     LLM model id (default: provider's default)
      --since duration   only consider sessions whose ended_at is within this window (default 168h0m0s)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
