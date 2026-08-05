## aichronicles unresolved

Print unresolved items from prior sessions in this cwd

### Synopsis

Reads each prior session's latest summary in --cwd (defaults
to $PWD), pulls $.unresolved from the body, and prints one
line per item with the source session id and topic. The
output is shaped to be drop-in usable as a Claude Code
SessionStart hook — pipe stdout into the agent's context so
the new session picks up where prior ones left off.

Talks to aichronicles-api over its UDS (override with
--socket or $AICHRONICLES_API_SOCKET).

Filters: --since (default 30d), --max-sessions (default 5),
--max-items (default 5 per session). Output is empty (0
exit) when no unresolved items match — a hook can pipe
straight in without a length check.

Use --format=json for the structured form when wiring this
into something other than a context-injection hook.

```
aichronicles unresolved [flags]
```

### Options

```
      --cwd string         cwd to look up (defaults to $PWD)
      --format string      output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help               help for unresolved
      --max-items int      cap on the number of unresolved items per session (default 5)
      --max-sessions int   cap on the number of prior sessions to draw from (default 5)
      --since duration     only consider sessions whose ended_at is within this window (e.g. 7d, 30d) (default 30d)
      --socket string      aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
