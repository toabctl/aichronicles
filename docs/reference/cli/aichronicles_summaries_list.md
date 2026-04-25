## aichronicles summaries list

List recent stored LLM outputs

### Synopsis

Prints stored llm_outputs rows newest-first. Without flags, it
shows the latest 50 across every session and every output type.
Filter with --session (prefix OK, same rules as `summarize`),
--type (summary | reflect | propose), or both.

Topic column is extracted from the stored JSON body when
possible; rows whose body is not parseable as a known type
show `(unparseable)` so the row is still discoverable by id.

```
aichronicles summaries list [flags]
```

### Options

```
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help             help for list
      --limit int        max rows to list (default 50)
      --session string   filter by session id or unique prefix
      --type string      filter by output type (summary | reflect | propose)
```

### SEE ALSO

* [aichronicles summaries](./aichronicles_summaries.md)	 - Inspect stored LLM outputs (summaries, reflections, proposals)
