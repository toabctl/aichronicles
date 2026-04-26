## aichronicles web

Serve a local web UI for browsing sessions and summaries

### Synopsis

Starts a small HTTP server on localhost that lists captured
sessions, surfaces cached LLM summaries, and exposes the same
FTS5 search the CLI uses. Reads the SQLite store directly in
read-only mode — does not go through the daemon's UDS, does
not write.

Default bind is 127.0.0.1; pass --bind to change. Binding to
a non-loopback address surfaces a startup warning. The server
has no authentication; the localhost-only boundary is the
trust model, mirroring the daemon's 0600 UDS.

```
aichronicles web [flags]
```

### Options

```
      --bind string   address to listen on (loopback by default; set to 0.0.0.0 for LAN access) (default "127.0.0.1")
      --db string     SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help          help for web
      --port int      port to listen on (default 7878)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
