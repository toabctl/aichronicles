## aichronicles import-jsonl

Replay events.jsonl into the SQLite store

### Synopsis

Reads a JSONL file of ingest envelopes (typically the POC's
events.jsonl) and inserts each line into the store. Idempotent:
duplicates (by event_id) are counted and skipped.

Use this once after upgrading from the JSONL-only POC to backfill
historical events into SQLite, or to replay a backup.

Trust model: import-jsonl bypasses the daemon. The store still
refuses unredacted envelopes (ErrRedactionRequired), but anything
the daemon would otherwise enforce — future origin signing, rate
limits, audit logging — does not run. Treat the input file as
authoritative; if a third party hands you events.jsonl, audit it
with `aichronicles audit` after import.

```
aichronicles import-jsonl <path> [flags]
```

### Options

```
      --db string   SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help        help for import-jsonl
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
