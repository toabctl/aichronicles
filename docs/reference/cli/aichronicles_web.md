## aichronicles web

Serve a local web UI for browsing sessions and summaries

### Synopsis

Starts a small HTTP server on localhost that lists captured
sessions, surfaces cached LLM summaries, and exposes the same
FTS5 search the CLI uses. Reads pass through the aichronicles-api
daemon's UDS via internal/apiclient — the daemon stays the only
process that opens the SQLite file. Runs as its own service
(aichronicles-web.service) so a wedged template or runaway view
query can't tear down the ingest worker.

Default bind is 127.0.0.1; pass --bind to change. Binding to
a non-loopback address surfaces a startup warning.

There is NO authentication. Be precise about what the bind
does and does not buy: it stops other machines, but a
loopback TCP port is readable by every local user, unlike
the daemon's 0600 UDS which the kernel restricts to one uid.
On a shared host, treat the corpus as readable by anyone
with an account. Requests are additionally Host-checked in
process, so a web page you visit cannot reach the UI by
re-resolving its own hostname to 127.0.0.1.

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
  -h, --help                    help for web
      --idle-timeout duration   shut down after this long with zero open connections (0 = no auto-shutdown when launched directly; defaults to 5m under systemd socket activation)
      --port int                port to listen on (ignored under systemd socket activation) (default 7878)
      --socket string           aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET; defaults to $XDG_RUNTIME_DIR/aichronicles/api.sock)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
