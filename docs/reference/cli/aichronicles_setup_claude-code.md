## aichronicles setup claude-code

Install Claude Code hooks + the aichronicles MCP server entry

### Synopsis

Two changes to ~/.claude/settings.json, both idempotent:

  1. Hooks: merges six entries (UserPromptSubmit, Stop,
     PostToolUse, PostToolUseFailure, SessionStart, SessionEnd)
     each pointing at `aichronicles ingest`.
  2. MCP server: registers an mcpServers.aichronicles entry
     pointing at `aichronicles mcp-serve`, so Claude can
     query past sessions / cached summaries / insights /
     skills / staleness mid-conversation.

Existing hook + MCP entries from other tools are preserved.
Pass --skip-mcp if you don't want the MCP server registered.

```
aichronicles setup claude-code [flags]
```

### Options

```
      --command string       command to run from each hook (default "aichronicles ingest")
  -h, --help                 help for claude-code
      --mcp-command string   command to register as the aichronicles MCP server (default "aichronicles")
      --settings string      path to Claude Code settings.json (default: ~/.claude/settings.json)
      --skip-mcp             do not register the aichronicles MCP server in settings.json
```

### SEE ALSO

* [aichronicles setup](./aichronicles_setup.md)	 - Install aichronicles into an AI coding agent or the OS
