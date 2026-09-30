## aichronicles summaries show

Show the most recent stored LLM output for a session

### Synopsis

Renders the latest summary stored for the given session
(prefix OK). Pass --format=json to emit the raw JSON body
instead of the human-readable render — useful for piping into
`jq`.

Errors with `no summary output for session …` when the session
exists but has never been summarized. Reflect and propose
outputs span many sessions and are not attached to one; list
them with `aichronicles summaries list --type reflect|propose`.

Talks to aichronicles-api over its UDS (override with
--socket or $AICHRONICLES_API_SOCKET).

```
aichronicles summaries show <session> [flags]
```

### Options

```
      --format string   output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help            help for show
      --socket string   aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
      --type string     output type (only summary is per-session) (default "summary")
```

### SEE ALSO

* [aichronicles summaries](./aichronicles_summaries.md)	 - Inspect stored LLM outputs (summaries, reflections, proposals)
