## aichronicles audit

Scan stored events for credential patterns (read-only)

### Synopsis

Runs the current credential detectors against every stored
event and reports matches. Use it to find leaks that predate
the redactor, or to validate that a new detector catches what
you expect. This command never modifies the store — see
`aichronicles scrub` for that.

```
aichronicles audit [flags]
```

### Options

```
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help             help for audit
      --limit int        max events to scan, newest first (0 = scan all)
      --since duration   only scan events with ts_source newer than this duration (e.g. 168h)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
