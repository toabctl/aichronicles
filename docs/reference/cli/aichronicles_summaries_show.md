## aichronicles summaries show

Show the most recent stored LLM output for a session

### Synopsis

Renders the latest llm_outputs row matching the given session
(prefix OK) and type (default: summary). Pass --json to emit the
raw JSON body instead of the human-readable render — useful for
piping into `jq`.

Errors with `no output for session …/type …` when the session
exists but has never been summarized/reflected/proposed under
the requested type.

```
aichronicles summaries show <session> [flags]
```

### Options

```
      --db string     SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help          help for show
      --json          emit raw JSON body instead of the human-readable render
      --type string   output type (summary | reflect | propose) (default "summary")
```

### SEE ALSO

* [aichronicles summaries](./aichronicles_summaries.md)	 - Inspect stored LLM outputs (summaries, reflections, proposals)
