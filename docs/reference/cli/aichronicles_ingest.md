## aichronicles ingest

Read a hook payload on stdin and forward as an envelope

### Synopsis

ingest is invoked by Claude Code hooks. It reads a JSON hook
payload from stdin, wraps it in the canonical Envelope, and POSTs
to aichroniclesd over a Unix socket.

Blocking policy: this command NEVER fails the hook. Errors are
logged to stderr as structured records and the process exits 0.

```
aichronicles ingest [flags]
```

### Options

```
  -h, --help            help for ingest
      --socket string   daemon UDS path (overrides $AICHRONICLES_SOCKET; defaults to XDG_RUNTIME_DIR)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
