## aichronicles import-claude

Walk Claude Code .jsonl transcripts and stream them into aichronicles-api

### Synopsis

Walks the directory at <path> looking for Claude Code session
transcripts (.jsonl files) and streams every conversational
line through POST /v1/import. The api applies server-side
redaction, runs the extractor registry, and writes through the
SQLite Sink — same path as live ingest. Idempotent on event_id.

<path> may be a single .jsonl or a directory; directories are
walked recursively. Use this once after upgrading to backfill
the user's prior Claude Code history into the store.

```
aichronicles import-claude <path> [flags]
```

### Options

```
  -h, --help            help for import-claude
      --socket string   aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
