## aichronicles teardown codex-cli

Remove aichronicles Codex CLI hooks from hooks.json

### Synopsis

Inverse of `setup codex-cli`. Strips every hook entry whose
command matches ours from each Codex hook event. Other tools'
entries are preserved; running twice is a no-op.

Dry-run by default: pass --yes to actually rewrite the file.
Does not touch ~/.codex/config.toml — flip
`[features] codex_hooks = false` yourself if you want to
disable hooks entirely.

```
aichronicles teardown codex-cli [flags]
```

### Options

```
      --command string    command to strip from each hook (default "aichronicles ingest --agent codex")
  -h, --help              help for codex-cli
      --settings string   path to Codex hooks.json (default: ~/.codex/hooks.json)
      --yes               confirm the removal (required to modify the file)
```

### SEE ALSO

* [aichronicles teardown](./aichronicles_teardown.md)	 - Remove aichronicles integration from an AI coding agent or the OS
