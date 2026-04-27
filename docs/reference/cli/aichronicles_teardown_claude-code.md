## aichronicles teardown claude-code

Remove aichronicles Claude Code hooks + MCP server entry

### Synopsis

Strips both halves of the install:

  - hooks from ~/.claude/settings.json (every entry whose
    command matches ours; entries from other tools survive)
  - mcpServers.aichronicles from ~/.claude.json

Empty event arrays + empty `hooks` / `mcpServers` containers
are cleaned up so the files look pristine after a full removal.
Idempotent: running twice is a no-op.

Runs in dry-run mode by default: it reports what would change
without touching either file. Pass --yes to actually write.

```
aichronicles teardown claude-code [flags]
```

### Options

```
      --command string       command to strip from each hook (default "aichronicles ingest")
  -h, --help                 help for claude-code
      --settings string      path to Claude Code settings.json for HOOKS (default: ~/.claude/settings.json)
      --user-config string   path to Claude Code user-config json for MCP SERVERS (default: ~/.claude.json)
      --yes                  confirm the removal (required to modify the files)
```

### SEE ALSO

* [aichronicles teardown](./aichronicles_teardown.md)	 - Remove aichronicles integration from an AI coding agent or the OS
