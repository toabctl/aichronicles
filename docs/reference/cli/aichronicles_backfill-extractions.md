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

Safe to run while the daemon is up — each batch is committed
in its own transaction.

```
aichronicles backfill-extractions [flags]
```

### Options

```
      --db string     SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help          help for backfill-extractions
      --only string   only rebuild this extraction kind (e.g. skill_load); empty = all kinds
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
