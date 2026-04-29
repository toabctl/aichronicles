## aichronicles propose discard

Mark a proposed skill as discarded (AutoSkill action 'discard')

### Synopsis

Records the AutoSkill (Yang et al., 2026 — arXiv:2603.01145)
maintenance action 'discard' on a candidate the user does
not want — neither added to disk nor merged into anything.
Future propose runs see this as an explicit rejection and
will bias away from re-suggesting near-duplicates.

No filesystem I/O. The skill_candidates row's decision
flips to 'discard' with the current timestamp; nothing
on disk changes.

```
aichronicles propose discard --skill <name> [flags]
```

### Options

```
      --db string       SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help            help for discard
      --output-id int   specific llm_outputs row id (default: latest propose row)
      --skill string    name of a skill from the proposal to discard
```

### SEE ALSO

* [aichronicles propose](./aichronicles_propose.md)	 - LLM-suggested skills / CLAUDE.md entries / scripts from recent sessions
