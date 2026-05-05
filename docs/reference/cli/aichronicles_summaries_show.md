## aichronicles summaries show

Show the most recent stored LLM output for a session

### Synopsis

Renders the latest llm_outputs row matching the given session
(prefix OK) and type (default: summary). Pass --format=json to
emit the raw JSON body instead of the human-readable render —
useful for piping into `jq`.

Errors with `no output for session …/type …` when the session
exists but has never been summarized/reflected/proposed under
the requested type.

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
      --type string     output type (summary | reflect | propose) (default "summary")
```

### SEE ALSO

* [aichronicles summaries](./aichronicles_summaries.md)	 - Inspect stored LLM outputs (summaries, reflections, proposals)
