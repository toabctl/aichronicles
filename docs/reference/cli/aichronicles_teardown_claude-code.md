## aichronicles teardown claude-code

Remove aichronicles Claude Code hooks from settings.json

### Synopsis

Strips every hook entry whose command matches ours from each
of the event types aichronicles installed into. Entries from other
tools are preserved unchanged. Empty event arrays and an empty
`hooks` object are cleaned up so the file looks pristine after
a full removal. Idempotent: running twice is a no-op.

Runs in dry-run mode by default: it reports what would change
without touching settings.json. Pass --yes to actually write.

```
aichronicles teardown claude-code [flags]
```

### Options

```
      --command string    command to strip from each hook (default "aichronicles ingest")
  -h, --help              help for claude-code
      --settings string   path to Claude Code settings.json (default: ~/.claude/settings.json)
      --yes               confirm the removal (required to modify settings.json)
```

### SEE ALSO

* [aichronicles teardown](./aichronicles_teardown.md)	 - Remove aichronicles integration from an AI coding agent or the OS
