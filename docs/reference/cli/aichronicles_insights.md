## aichronicles insights

Cross-session usage digest (sessions, top tools, top skills, activity-by-hour)

### Synopsis

Reads sessions, events, and skill_load extractions in a window
and prints an aggregated report: counters (sessions, events,
tool calls), top tools, top skills, an activity-by-hour
histogram, and the highest-event-count sessions.

No LLM call — pure SQL aggregation, fast even on large stores.
For LLM-derived analysis, see `reflect` and `propose`.

Talks to aichronicles-api over its UDS (override with
--socket or $AICHRONICLES_API_SOCKET).

```
aichronicles insights [flags]
```

### Options

```
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for insights
      --since duration   only consider sessions/events within this window (e.g. 24h, 7d, 30d) (default 720h0m0s)
      --socket string    aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
