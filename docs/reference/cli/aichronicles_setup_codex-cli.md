## aichronicles setup codex-cli

Install Codex CLI hooks that forward events to aichroniclesd

### Synopsis

Idempotently merges hook entries (UserPromptSubmit, Stop,
PostToolUse, PostToolUseFailure) into ~/.codex/hooks.json,
each pointing at `aichronicles ingest --agent codex`. Existing
hook entries from other tools are preserved; running twice is
a no-op.

Codex hooks must be enabled separately by setting
`[features] codex_hooks = true` in ~/.codex/config.toml — this
command does not edit your config.toml; it only writes hooks.json.

```
aichronicles setup codex-cli [flags]
```

### Options

```
      --command string    command to run from each hook (default "aichronicles ingest --agent codex")
  -h, --help              help for codex-cli
      --settings string   path to Codex hooks.json (default: ~/.codex/hooks.json)
```

### SEE ALSO

* [aichronicles setup](./aichronicles_setup.md)	 - Install aichronicles into an AI coding agent or the OS
