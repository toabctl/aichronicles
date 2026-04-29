## aichronicles propose merge

Merge a proposed skill into the existing on-disk SKILL.md (AutoSkill action 'merge')

### Synopsis

Loads the latest cached `propose` output (or the one
identified by --output-id), finds <name> in it, and merges
that candidate into the existing ~/.claude/skills/<name>/
SKILL.md per the AutoSkill (Yang et al., 2026 —
arXiv:2603.01145) maintenance rules: preserve the original
capability identity, semantic union not raw concatenation,
no regressions.

Bumps the patch component of the existing SKILL.md's
version field (v0.1.0 → v0.1.1). Records the lifecycle
transition on the candidate's skill_candidates row
(decision='merge', merged_into_id=existing-candidate-id).

Verification gate runs by default — same critic that
`propose add` invokes. Pass --no-verify to bypass.

```
aichronicles propose merge --skill <name> [flags]
```

### Options

```
      --db string           SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help                help for merge
      --no-verify           skip the critic-LLM verification gate
      --output-id int       specific llm_outputs row id (default: latest propose row)
      --skill string        name of a skill from the proposal to merge into its on-disk twin
      --skills-dir string   override target directory (default: ~/.claude/skills)
```

### SEE ALSO

* [aichronicles propose](./aichronicles_propose.md)	 - LLM-suggested skills / CLAUDE.md entries / scripts from recent sessions
