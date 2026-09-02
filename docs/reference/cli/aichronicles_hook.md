## aichronicles hook

Read a hook payload on stdin and forward as an envelope to aichronicles-api

### Synopsis

hook is invoked by AI coding agent hooks (Claude Code by
default; pass --agent gemini-cli or --agent codex-cli to
consume Gemini CLI / Codex CLI hook payloads). It reads a
JSON hook payload from stdin, wraps it in the canonical
Envelope, and POSTs to aichronicles-api over
a Unix socket. The api daemon applies redaction server-side.

Blocking policy: this command NEVER fails the hook. Errors are
logged to stderr as structured records and the process exits 0.

```
aichronicles hook [flags]
```

### Options

```
      --agent string    source agent slug (claude-code | gemini-cli | codex-cli) (default "claude-code")
  -h, --help            help for hook
      --socket string   aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET; defaults to XDG_RUNTIME_DIR/aichronicles/api.sock)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
