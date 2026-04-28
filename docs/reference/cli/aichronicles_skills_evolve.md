## aichronicles skills evolve

Draft a revised SKILL.md for a stale-correlated skill, grounded in captured failures

### Synopsis

Reads ~/.claude/skills/<name>/SKILL.md and the failure events
the staleness detector found correlated with this skill, then
asks the LLM to revise the SKILL: tighten the trigger, add
pitfalls, fix concrete instruction errors. The frontmatter is
preserved verbatim — the SKILL keeps its identity.

Output lands at ~/.claude/skills/<name>/SKILL.md.v2 — the
original is left alone. Diff the two and replace manually if
the revision looks good. The LLM may also decide no revision
is warranted (failures look unrelated, evidence too thin) and
return a no-change verdict instead.

Implements gap #4 from the research-vs-aichronicles comparison
memory: the TDS Voyager critique notes that Voyager-style
systems flag stale skills but rarely act on them — this
command is the act-on-it side.

Cached under kind=skill_revision keyed on the SKILL's
content-hash + name, so re-running on an unchanged SKILL is
free; hand-editing the SKILL invalidates the cache.

Requires the LLM provider to be configured (same as
summarize/reflect/propose).

```
aichronicles skills evolve --skill <name> [flags]
```

### Options

```
      --db string           SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --examples int        how many failure examples to include in the prompt (default 5)
      --force               bypass the cache and re-call the LLM even if a revision was already drafted
  -h, --help                help for evolve
      --model string        LLM model id (default: provider's default)
      --since duration      only consider failures within this window (e.g. 14d, 30d) (default 720h0m0s)
      --skill string        name of the SKILL to revise (must exist under --skills-dir)
      --skills-dir string   override target directory (default: ~/.claude/skills)
      --window duration     how long after a skill load to look for a tool_failure (e.g. 5m, 10m) (default 10m0s)
```

### SEE ALSO

* [aichronicles skills](./aichronicles_skills.md)	 - Inspect captured skill activity (frequency, staleness, ...)
