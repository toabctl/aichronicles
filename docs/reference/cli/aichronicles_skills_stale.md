## aichronicles skills stale

Surface skills whose loads correlate with subsequent tool_failures

### Synopsis

Walks every skill_load extraction in the window and flags
skills where a load was followed by a tool_failure event in
the same session within --window. The signal is conservative:
only Claude's PostToolUseFailure hook fills tool_failure events,
so a low rate doesn't mean the skill is healthy — just that
this signal hasn't fired. A consistently high rate is a strong
hint that the skill's instructions are wrong / outdated and
deserve a `skill_manage edit` pass.

Output is sorted most-likely-broken first.

```
aichronicles skills stale [flags]
```

### Options

```
      --db string         SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
      --format string     output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help              help for stale
      --since duration    only consider skill loads within this window (e.g. 24h, 7d, 30d) (default 336h0m0s)
      --window duration   how long after a skill load to look for a tool_failure (e.g. 5m, 10m, 30m) (default 10m0s)
```

### SEE ALSO

* [aichronicles skills](./aichronicles_skills.md)	 - Inspect captured skill activity (frequency, staleness, ...)
