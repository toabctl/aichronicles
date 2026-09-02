## aichronicles teardown codex-cli

Remove aichronicles Codex CLI hooks from hooks.json

### Synopsis

Inverse of `setup codex-cli`. Strips every hook entry whose
command matches ours from each Codex hook event. Other tools'
entries are preserved; running twice is a no-op.

Default path is $CODEX_HOME/hooks.json, or ~/.codex/hooks.json
when CODEX_HOME is unset.

Codex's own trust records (the [hooks.state] table in
config.toml, keyed by file path + event + index) are left
alone: they are Codex's bookkeeping, not ours, and a stale
entry for a hook that no longer exists is inert.

Dry-run by default: pass --yes to actually rewrite the file.

```
aichronicles teardown codex-cli [flags]
```

### Options

```
      --command string    command to strip from each hook (default "aichronicles hook --agent codex-cli")
  -h, --help              help for codex-cli
      --settings string   path to Codex hooks.json (default: $CODEX_HOME/hooks.json, else ~/.codex/hooks.json)
      --yes               confirm the removal (required to modify the file)
```

### SEE ALSO

* [aichronicles teardown](./aichronicles_teardown.md)	 - Remove aichronicles integration from an AI coding agent or the OS
