## aichronicles setup claude-code

Install Claude Code hooks that forward events to aichroniclesd

### Synopsis

Idempotently merges six hook entries (UserPromptSubmit, Stop,
PostToolUse, PostToolUseFailure, SessionStart, SessionEnd) into
the target settings.json, each pointing at `aichronicles ingest`.
Existing hook entries from other tools are preserved; running
twice is a no-op.

```
aichronicles setup claude-code [flags]
```

### Options

```
      --command string    command to run from each hook (default "aichronicles ingest")
  -h, --help              help for claude-code
      --settings string   path to Claude Code settings.json (default: ~/.claude/settings.json)
```

### SEE ALSO

* [aichronicles setup](./aichronicles_setup.md)	 - Install aichronicles into an AI coding agent or the OS
