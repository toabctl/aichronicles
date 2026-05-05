## aichronicles import-jsonl

Replay events.jsonl into the SQLite store via aichronicles-api

### Synopsis

Streams a JSONL file of ingest envelopes (typically the POC's
events.jsonl) into POST /v1/import on aichronicles-api.
Idempotent: duplicates (by event_id) are counted and skipped.

Use this once after upgrading from the JSONL-only POC to backfill
historical events into SQLite, or to replay a backup.

Trust model: the api applies server-side redaction to every line
regardless of any redaction.applied claim, so a third-party
events.jsonl can be imported safely. After import, run
`aichronicles audit` to inspect anything the redactor missed.

```
aichronicles import-jsonl <path> [flags]
```

### Options

```
  -h, --help            help for import-jsonl
      --socket string   aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
