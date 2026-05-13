## aichronicles backfill-extractions

Re-run extractors over every raw envelope and rewrite the extractions table

### Synopsis

Walks every row in raw_envelopes, deserialises envelope_json,
and rewrites the extractions table to match what the current
set of extractors would produce. Use this when a new extractor
lands (skill_load, web_query, …) and you want it applied to
events ingested before it existed — without wiping the store
and re-importing.

Idempotent. With --only=<kind>, only rows matching that kind
are deleted/replaced; other kinds are left untouched.
Without --only, ALL extraction rows are rebuilt from scratch.

Refuses to run while aichronicles-api is up: this command
rewrites the daemon-owned extractions table and racing the
IngestWorker would leave inconsistent rows. Stop the
daemon first (systemctl --user stop aichronicles-api),
run this, then restart.

```
aichronicles backfill-extractions [flags]
```

### Options

```
      --db string       SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help            help for backfill-extractions
      --only string     only rebuild this extraction kind (e.g. skill_load); empty = all kinds
      --socket string   aichronicles-api UDS path used to check whether the daemon is running (overrides $AICHRONICLES_API_SOCKET; XDG default)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
