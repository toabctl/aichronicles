## aichronicles import-jsonl

Replay events.jsonl into the SQLite store

### Synopsis

Reads a JSONL file of ingest envelopes (typically the POC's
events.jsonl) and inserts each line into the store. Idempotent:
duplicates (by event_id) are counted and skipped.

Use this once after upgrading from the JSONL-only POC to backfill
historical events into SQLite, or to replay a backup.

```
aichronicles import-jsonl <path> [flags]
```

### Options

```
      --db string   SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)
  -h, --help        help for import-jsonl
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
