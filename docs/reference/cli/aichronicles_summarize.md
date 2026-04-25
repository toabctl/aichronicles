## aichronicles summarize

Generate an LLM summary for one session

### Synopsis

Pulls every event for the given session, asks the LLM for a
structured summary (topic, what was done, unresolved issues,
files touched, annotated links), and persists the JSON reply
in llm_outputs. Session id may be a unique prefix (see
`aichronicles sessions`).

Idempotent on the full prompt: re-running without --force returns
the cached summary and does not call the LLM again. Pass --force
to bypass the cache (e.g. after changing the prompt template).

Output is rendered for the terminal by default; pass
--format=json to emit the raw JSON body stored in the
database.

Requires ANTHROPIC_API_KEY to be set unless --force is off AND
a cached summary exists.

```
aichronicles summarize <session> [flags]
```

### Options

```
      --db string       SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --force           bypass the llm_outputs cache and re-call the LLM
      --format string   output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help            help for summarize
      --model string    LLM model id (default: provider's default)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
