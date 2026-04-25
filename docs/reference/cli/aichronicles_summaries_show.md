## aichronicles summaries show

Show the most recent stored LLM output for a session

### Synopsis

Renders the latest llm_outputs row matching the given session
(prefix OK) and kind (default: summary). Pass --json to emit the
raw JSON body instead of the human-readable render — useful for
piping into `jq`.

Errors with `no output for session …/kind …` when the session
exists but has never been summarized/reflected/proposed under
the requested kind.

```
aichronicles summaries show <session> [flags]
```

### Options

```
      --db string     SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)
  -h, --help          help for show
      --json          emit raw JSON body instead of the human-readable render
      --kind string   output kind (summary | reflect | propose; default: summary)
```

### SEE ALSO

* [aichronicles summaries](./aichronicles_summaries.md)	 - Inspect stored LLM outputs (summaries, reflections, proposals)
