## aichronicles vacuum

Compact the SQLite store and truncate the WAL

### Synopsis

Runs PRAGMA wal_checkpoint(TRUNCATE) followed by VACUUM. The
checkpoint flushes pending WAL frames into the main DB so VACUUM
sees current state; VACUUM then rewrites the DB into a temp file
and renames it, releasing freelist pages back to the filesystem.

Caveats:
  - VACUUM blocks concurrent writers (readers in WAL mode are fine).
    The daemon is a writer; consider stopping it during a vacuum on
    a heavily-active store.
  - VACUUM needs ~2× the DB size in free disk during the rewrite.
  - Pass --yes to actually run; default is a no-op preview that
    prints the current page count.

```
aichronicles vacuum [flags]
```

### Options

```
  -h, --help            help for vacuum
      --socket string   aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
      --yes             actually vacuum; without --yes the command prints current size and exits
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
