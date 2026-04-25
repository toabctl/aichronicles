## aichronicles summarize

Generate an LLM summary for one session

### Synopsis

Pulls every event for --session, asks the LLM for a structured
summary (topic, what was done, unresolved issues, files touched,
annotated links), and persists the JSON reply in llm_outputs.

Idempotent on the full prompt: re-running without --force returns
the cached summary and does not call the LLM again. Pass --force
to bypass the cache (e.g. after changing the prompt template).

Output is rendered for the terminal by default; pass --json to
emit the raw JSON body stored in the database.

Requires ANTHROPIC_API_KEY to be set unless --force is off AND
a cached summary exists.

```
aichronicles summarize [flags]
```

### Options

```
      --db string                       SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)
      --force                           bypass the llm_outputs cache and re-call the LLM
  -h, --help                            help for summarize
      --json                            emit raw JSON body instead of the human-readable render
      --model string                    LLM model id (default: provider's default)
      --session aichronicles sessions   session id or unique prefix (see aichronicles sessions)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
