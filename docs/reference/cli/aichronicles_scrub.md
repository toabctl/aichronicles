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

Talks to aichronicles-api over its UDS so the scrub holds the
single SQLite writer lock cleanly (no contention with the live
ingest path).

```
aichronicles scrub [flags]
```

### Options

```
  -h, --help            help for scrub
      --socket string   aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
      --yes             confirm irreversible writes (required to mutate the DB)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
