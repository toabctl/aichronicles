## aichronicles facts induce

Induce typed facts from one specific session

### Synopsis

Asks the LLM to extract typed (subject, predicate, object)
triples from the named session — project-level facts the
future agent benefits from knowing without re-discovery.

Persists the LLM reply in llm_outputs(kind=facts) AND each
individual fact in semantic_facts (the typed retrieval
surface). Re-running on the same session hits the cache;
--force re-calls the LLM. Re-asserting the same fact
upserts in place; conflicting fact objects coexist as
separate rows.

The session must have been summarized first.
Requires ANTHROPIC_API_KEY unless the cache hits.

```
aichronicles facts induce --session <id> [flags]
```

### Options

```
      --force            bypass the llm_outputs cache and re-call the LLM
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for induce
      --model string     LLM model id (default: provider's default)
      --session string   session id (full or unique prefix) to induce facts from
      --socket string    aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
```

### SEE ALSO

* [aichronicles facts](./aichronicles_facts.md)	 - Typed semantic-fact memory induced from sessions (MIRIX semantic layer)
