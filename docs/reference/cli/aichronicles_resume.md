## aichronicles resume

Search sessions and resume the chosen one in its workspace

### Synopsis

Full-text searches captured sessions for <query>, lists the
best matches one per line (when, short id, cwd, opening
prompt), and prompts you to pick one. The chosen session is
resumed by launching its agent (`claude --resume <id>` /
`gemini --resume <id>`) after cd-ing into the workspace the
session started in — `claude --resume` indexes transcripts by
start cwd, so this is what makes resume actually find the
conversation.

The current process is replaced by the agent, so you land
directly in the resumed session. Pass --print to emit the
resume one-liners instead of launching (also the automatic
behavior when stdin is not a terminal, so it composes with
pipes). Sessions whose agent we can't model are omitted —
resume only lists what it can actually relaunch.

By default only sessions active in the last 6 weeks are
considered; widen or disable the window with --since (e.g.
--since 90d, or --since 0 for no limit).

Talks to aichronicles-api over its UDS (override with
--socket or $AICHRONICLES_API_SOCKET).

```
aichronicles resume <query> [flags]
```

### Options

```
      --agent string       filter by source agent (claude-code | gemini-cli)
  -h, --help               help for resume
      --limit int          max matching sessions to list (default 10)
  -n, --print              print the resume command(s) instead of launching the agent
      --since duration     only sessions with events within this window (e.g. 24h, 7d); 0 = no limit (default 42d)
  -d, --skip-permissions   (dangerous) resume with --dangerously-skip-permissions (claude-code only)
      --socket string      aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
