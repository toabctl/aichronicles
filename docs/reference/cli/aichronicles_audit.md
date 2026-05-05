## aichronicles audit

Scan stored events for credential patterns (read-only)

### Synopsis

Asks aichronicles-api to run the current credential detectors
against every stored event and prints one row per match. Use it
to find leaks that predate the redactor, or to validate that a
new detector catches what you expect. This command never
modifies the store — see `aichronicles scrub` for that.

The api runs the scanner server-side and returns the marker
form of every match — raw secret bytes never traverse the wire,
so audit output is safe to paste into a ticket.

Pass --format=json for a structured payload suitable for jq.

Talks to aichronicles-api over its UDS (override with
--socket or $AICHRONICLES_API_SOCKET).

```
aichronicles audit [flags]
```

### Options

```
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for audit
      --limit int        max events to scan, newest first (0 = scan all)
      --since duration   only scan events with ts_source newer than this duration (e.g. 24h, 7d) (default 0s)
      --socket string    aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
