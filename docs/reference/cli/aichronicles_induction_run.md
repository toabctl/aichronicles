## aichronicles induction run

Induce on one specific session id

### Synopsis

Same prompt and persistence as `induction sweep`, but for a
single session you name explicitly. Useful for replaying or
force-recomputing — pair with --force to bypass the cache.

Requires ANTHROPIC_API_KEY unless the cache hits.

```
aichronicles induction run --session <id> [flags]
```

### Options

```
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --force            bypass the cache and re-call the LLM
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for run
      --model string     LLM model id (default: provider's default)
      --session string   session id (full or unique prefix) to induce on
```

### SEE ALSO

* [aichronicles induction](./aichronicles_induction.md)	 - Online single-session induction (AWM-style auto-skill-extraction)
