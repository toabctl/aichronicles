## aichronicles facts show

Show every typed fact known about one subject

### Synopsis

Loads every semantic_facts row for the given subject. Use
the cwd path of a project to get its build/test/deploy
contract — this is the v1 retrieval surface for typed
semantic memory.


```
aichronicles facts show --subject <cwd|name> [flags]
```

### Options

```
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for show
      --limit int        max facts to return (default 100)
      --subject string   subject (cwd path or other anchor) to load facts for
```

### SEE ALSO

* [aichronicles facts](./aichronicles_facts.md)	 - Typed semantic-fact memory induced from sessions (MIRIX semantic layer)
