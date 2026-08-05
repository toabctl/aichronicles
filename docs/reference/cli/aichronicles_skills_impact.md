## aichronicles skills impact

Per-skill success rate over recent loads (positive view of the staleness signal)

### Synopsis

Walks every skill_load extraction in the window and reports,
per skill, how many loads were followed by a tool_failure in
the same session within --window vs how many were not. Where
`skills stale` surfaces only the trouble skills, `skills impact`
shows the FULL distribution — including the 100%-success ones
— so you can see which skills are actually pulling their weight
and which are pure noise. The same signal feeds the propose
prompt's installed-skills enrichment so the model gets to
reason about success rates when deciding whether a new skill
is warranted (or whether an existing skill should be revised
instead).

The signal is conservative: only Claude's PostToolUseFailure
hook fills tool_failure events, so a high success rate doesn't
mean the skill is perfect — just that this signal hasn't
fired. Output is sorted most-loaded first.

Talks to aichronicles-api over its UDS (override with
--socket or $AICHRONICLES_API_SOCKET).

```
aichronicles skills impact [flags]
```

### Options

```
      --format string     output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help              help for impact
      --since duration    only consider skill loads within this window (e.g. 24h, 7d, 30d) (default 30d)
      --socket string     aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
      --window duration   how long after a skill load to look for a tool_failure (e.g. 5m, 10m, 30m) (default 10m0s)
```

### SEE ALSO

* [aichronicles skills](./aichronicles_skills.md)	 - Inspect captured skill activity (frequency, staleness, ...)
