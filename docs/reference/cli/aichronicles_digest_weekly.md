## aichronicles digest weekly

Generate a weekly reflect digest, persisted with kind=reflect_weekly

### Synopsis

Computes the previous completed Monday-00:00-UTC →
Monday-00:00-UTC week and runs reflect over the sessions in
that window. Override the period with --week-of <YYYY-MM-DD>
to target a different Monday (the date you pass is anchored
to that week's Monday).

Re-running the same week is a cache hit (the period dates are
in the prompt's user message, so the prompt_hash naturally
differs across weeks but stays stable for a given week).
Pass --force to re-call the LLM.

Requires ANTHROPIC_API_KEY unless the cache hits.

```
aichronicles digest weekly [flags]
```

### Options

```
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --force            bypass the llm_outputs cache and re-call the LLM
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for weekly
      --model string     LLM model id (default: provider's default)
      --week-of string   target a specific Monday (YYYY-MM-DD); default is the previous completed week
```

### SEE ALSO

* [aichronicles digest](./aichronicles_digest.md)	 - Periodic LLM-driven digests stored as queryable artefacts
