## aichronicles summaries list

List recent stored LLM outputs

### Synopsis

Prints stored llm_outputs rows newest-first. Without flags, it
shows the latest 50 across every session and every kind. Filter
with --session (prefix OK, same rules as `summarize`), --kind
(summary | reflect | propose), or both.

Topic column is extracted from the stored JSON body when
possible; rows whose body is not parseable as a known kind
show `(unparseable)` so the row is still discoverable by id.

```
aichronicles summaries list [flags]
```

### Options

```
      --db string        SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)
  -h, --help             help for list
      --kind string      filter by kind (summary | reflect | propose)
      --limit int        max rows to list (default 50)
      --session string   filter by session id or unique prefix
```

### SEE ALSO

* [aichronicles summaries](./aichronicles_summaries.md)	 - Inspect stored LLM outputs (summaries, reflections, proposals)
