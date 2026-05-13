## aichronicles web

Serve a local web UI for browsing sessions and summaries

### Synopsis

Starts a small HTTP server on localhost that lists captured
sessions, surfaces cached LLM summaries, and exposes the same
FTS5 search the CLI uses. Reads the SQLite store directly via
WAL — does not go through the daemon's UDS. Runs as its own
process (aichronicles-web.service) so a wedged template or
runaway view query can't tear down the ingest worker.

Default bind is 127.0.0.1; pass --bind to change. Binding to
a non-loopback address surfaces a startup warning. The server
has no authentication; the localhost-only boundary is the
trust model, mirroring the daemon's 0600 UDS.

Socket activation: when launched by systemd via
aichronicles-web.socket (LISTEN_FDS in env), the server
adopts the inherited fd, ignores --bind/--port, and enables
idle auto-shutdown (default 5m of zero connections) so the
service exits between bursts and the .socket unit relaunches
it on the next request.

```
aichronicles web [flags]
```

### Options

```
      --bind string             address to listen on (loopback by default; set to 0.0.0.0 for LAN access; ignored under systemd socket activation) (default "127.0.0.1")
      --db string               SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help                    help for web
      --idle-timeout duration   shut down after this long with zero open connections (0 = no auto-shutdown when launched directly; defaults to 5m under systemd socket activation)
      --port int                port to listen on (ignored under systemd socket activation) (default 7878)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
