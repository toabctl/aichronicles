## aichronicles ingest

Read a hook payload on stdin and forward as an envelope

### Synopsis

ingest is invoked by AI coding agent hooks (Claude Code by
default; pass --agent codex to consume OpenAI Codex CLI hook
payloads). It reads a JSON hook payload from stdin, wraps it
in the canonical Envelope, and POSTs to aichroniclesd over a
Unix socket.

Blocking policy: this command NEVER fails the hook. Errors are
logged to stderr as structured records and the process exits 0.

```
aichronicles ingest [flags]
```

### Options

```
      --agent string    source agent slug (claude-code | codex) (default "claude-code")
  -h, --help            help for ingest
      --socket string   daemon UDS path (overrides $AICHRONICLES_SOCKET; defaults to XDG_RUNTIME_DIR)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
