## aichronicles scrub

Rewrite stored events to remove credentials (IRREVERSIBLE with --yes)

### Synopsis

Retroactive scrubber. For every stored event, runs the current
detectors and rewrites matches to <redacted:kind> markers in both
events.content_text and raw_envelopes.envelope_json.

Runs in dry-run mode by default: it reports what would change
without touching the database. Pass --yes to actually write.

This is IRREVERSIBLE. raw_envelopes is aichronicles' source-of-
truth layer; once rewritten, the original bytes are gone. Take a
backup of the DB file first if you care about forensics.

```
aichronicles scrub [flags]
```

### Options

```
      --db string   SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)
      --dry-run     report changes without writing (default: on unless --yes)
  -h, --help        help for scrub
      --yes         confirm irreversible writes (required to mutate the DB)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
