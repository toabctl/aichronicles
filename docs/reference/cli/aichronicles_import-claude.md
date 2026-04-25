## aichronicles import-claude

Import Claude Code's own ~/.claude transcripts into the store

### Synopsis

Walks one or more Claude Code transcript files (*.jsonl) and
ingests each conversational line (user/assistant/system) as an
envelope. Claude-internal bookkeeping rows (file-history-snapshot,
permission-mode, queue-operation, attachment, last-prompt) are
silently skipped — they carry no content we search.

event_id is Claude's per-entry uuid verbatim so re-imports are
idempotent and the stored row is greppable against the source
transcript. A missing or malformed uuid on a conversational row
is logged loudly and counted — we surface format drift rather
than hide it behind a synthesized ID.

path defaults to ~/.claude/projects. A specific file or directory
works too.

```
aichronicles import-claude [path] [flags]
```

### Options

```
      --db string   SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)
  -h, --help        help for import-claude
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
