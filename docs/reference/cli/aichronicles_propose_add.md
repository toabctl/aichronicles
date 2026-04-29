## aichronicles propose add

Add a proposed skill (SKILL.md + scripts) to disk (AutoSkill action 'add')

### Synopsis

Loads the latest cached `propose` output (or the one
identified by --output-id) and writes the named skill to
~/.claude/skills/<name>/. Includes:

  - SKILL.md with frontmatter (name, description, version,
    tags, triggers, examples — the AutoSkill 7-tuple) and a
    scaffolded body (When to use, Steps with TODO markers).
  - scripts/<name> for each helper script the proposal
    listed under the skill (chmod 0755, with shebang and
    purpose-comment header).

Verification gate (Voyager-style critic): before writing,
a second LLM pass evaluates the proposed skill against its
cited evidence and your installed skills. On a refusal
(near-duplicate of an installed skill, evidence too thin,
generic when_to_use, or fabricated steps) the add is
aborted with the critic's concern + recommendation. Pass
--no-verify to bypass the gate. The verification result is
cached as kind=propose_verify so re-running on the same
proposal is free.

All targets are refused if they already exist unless
--force is passed. Use `aichronicles propose list` to see
what's in the cached proposal. Use `aichronicles propose
merge --skill <name>` instead to fold the candidate into
an existing on-disk skill rather than creating a new one.

```
aichronicles propose add --skill <name> [flags]
```

### Options

```
      --db string           SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --force               overwrite existing target files
  -h, --help                help for add
      --no-verify           skip the critic-LLM verification gate (writes the SKILL without checking for duplicates / weak evidence)
      --output-id int       specific llm_outputs row id (default: latest propose row)
      --skill string        name of a skill from the proposal to materialise
      --skills-dir string   override target directory (default: ~/.claude/skills)
```

### SEE ALSO

* [aichronicles propose](./aichronicles_propose.md)	 - LLM-suggested skills / CLAUDE.md entries / scripts from recent sessions
