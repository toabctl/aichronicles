## aichronicles facts

Typed semantic-fact memory induced from sessions (MIRIX semantic layer)

### Synopsis

Facts are typed (subject, predicate, object) triples derived
from a session — project-level claims like 'uses Go 1.26',
'runs tests via go test ./...'. Distinct from skills /
workflows / propose (procedural memory): facts answer
'what is true?' rather than 'how do I do X?'. The retrieval
surface is keyed by subject (typically a cwd) so the next
session that opens in the same project can ground itself
without re-discovering the build/test/deploy contract from
raw events.

Subcommands:
  induce   — induce typed facts from one specific session id
  list     — show recent fact inductions (LLM rows)
  show     — show every fact for a given subject (e.g. cwd)


### Options

```
  -h, --help   help for facts
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
* [aichronicles facts induce](./aichronicles_facts_induce.md)	 - Induce typed facts from one specific session
* [aichronicles facts list](./aichronicles_facts_list.md)	 - Show recent fact inductions (one row per LLM run)
* [aichronicles facts show](./aichronicles_facts_show.md)	 - Show every typed fact known about one subject
