## aichronicles setup gemini-cli

Install Gemini CLI hooks that forward events to aichronicles-api

### Synopsis

Idempotently merges five hook entries (BeforeAgent, AfterModel,
AfterTool, SessionStart, SessionEnd) into the target
settings.json, each pointing at `aichronicles hook --agent
gemini-cli`. Existing hook entries from other tools are
preserved; running twice is a no-op.

Default settings path is ~/.gemini/settings.json (user-level
hooks). Pass --settings to target a project-local
<project>/.gemini/settings.json instead.

Gemini's hook protocol is a near-clone of Claude Code's: it
sends the same JSON-on-stdin shape, so the same `aichronicles
ingest` shim handles both. Tool failures are detected from
AfterTool's tool_response.error field rather than via a
separate event name.

```
aichronicles setup gemini-cli [flags]
```

### Options

```
      --command string    command to run from each hook (default "aichronicles hook --agent gemini-cli")
  -h, --help              help for gemini-cli
      --settings string   path to Gemini settings.json (default: ~/.gemini/settings.json)
```

### SEE ALSO

* [aichronicles setup](./aichronicles_setup.md)	 - Install aichronicles into an AI coding agent or the OS
