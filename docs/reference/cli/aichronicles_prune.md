## aichronicles prune

Delete sessions (and everything they own) older than --older-than

### Synopsis

Removes every session whose ended_at_ms is older than --older-than
and cascades to its raw_envelopes / events / extractions / events_fts
rows. Active sessions (ended_at NULL) are protected, regardless of how
old started_at is.

Cached LLM outputs (summaries, reflections, propose drafts) survive by
default — their session_id is set NULL via the schema's ON DELETE
SET NULL, so they remain as historical record without a parent. Pass
--include-llm-outputs to drop those too.

Default is dry-run: nothing is written until you pass --yes. Run
`aichronicles vacuum` afterwards to reclaim freelist pages on disk.

```
aichronicles prune [flags]
```

### Options

```
  -h, --help                  help for prune
      --include-llm-outputs   also delete llm_outputs rows older than the cutoff (summaries, reflections)
      --older-than duration   prune sessions whose ended_at is older than this (e.g. 30d, 180d, 24h) (default 4320h0m0s)
      --socket string         aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
      --yes                   actually delete; without --yes the command runs as dry-run
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
