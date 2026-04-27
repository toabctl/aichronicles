## aichronicles propose apply

Materialise a proposed skill (SKILL.md + scripts) on disk

### Synopsis

Loads the latest cached `propose` output (or the one
identified by --output-id) and writes the named skill to
~/.claude/skills/<name>/. Includes:

  - SKILL.md with frontmatter (name, description) and a
    scaffolded body (When to apply, Why, Steps/Pitfalls/
    Verification with TODO markers).
  - scripts/<name> for each helper script the proposal
    listed under the skill (chmod 0755, with shebang and
    purpose-comment header).

All targets are refused if they already exist unless
--force is passed. Use `aichronicles propose list` to see
what's in the cached proposal.

```
aichronicles propose apply --skill <name> [flags]
```

### Options

```
      --db string           SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --force               overwrite existing target files
  -h, --help                help for apply
      --output-id int       specific llm_outputs row id (default: latest propose row)
      --skill string        name of a skill from the proposal to materialise
      --skills-dir string   override target directory (default: ~/.claude/skills)
```

### SEE ALSO

* [aichronicles propose](./aichronicles_propose.md)	 - LLM-suggested skills / CLAUDE.md entries / scripts from recent sessions
