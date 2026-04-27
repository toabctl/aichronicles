## aichronicles embed

Compute and store vector embeddings for events lacking them

### Synopsis

Walks the events table, finds rows with no embedding for the
target model, and posts batched requests to the OpenAI
embeddings endpoint. The resulting float32 vectors land in
the event_embeddings table for `aichronicles search
--semantic` to score against.

Idempotent: re-running picks up where a previous run left
off (no row → embed; existing row for the same model →
skip). A model upgrade (text-embedding-3-small → -3-large)
can be done with `--model` set to the new id; old rows for
the previous model are left in place until you re-embed
or prune.

Requires OpenAI configured under [llm] (provider=openai).
Anthropic does not expose a hosted embeddings endpoint, so
this command refuses under provider=anthropic rather than
silently downgrading to a different vector space.

```
aichronicles embed [flags]
```

### Options

```
      --batch int        how many inputs per OpenAI embeddings request (default 64)
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --dry-run          print the work plan and exit without calling the API
  -h, --help             help for embed
      --kind strings     limit to event kinds (default: user_prompt, assistant_message, tool_use; pass empty to embed all) (default [user_prompt,assistant_message,tool_use])
      --limit int        cap the number of events embedded this run (0 = no cap; resume on next run)
      --model string     embedding model id (default: text-embedding-3-small)
      --since duration   only embed events from the last N (e.g. 7d, 24h) (default 0s)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
