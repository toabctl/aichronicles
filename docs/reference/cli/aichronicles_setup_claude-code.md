## aichronicles setup claude-code

Install Claude Code hooks + the aichronicles MCP server entry

### Synopsis

Two idempotent changes to TWO different files. Claude Code
keeps hook config and MCP-server config in different places:

  1. Hooks → ~/.claude/settings.json: merges six entries
     (UserPromptSubmit, Stop, PostToolUse, PostToolUseFailure,
     SessionStart, SessionEnd) each pointing at
     `aichronicles hook`.
  2. MCP server → ~/.claude.json: registers
     mcpServers.aichronicles pointing at `aichronicles
     mcp-serve`, so Claude can query past sessions / cached
     summaries / insights / skills / staleness mid-conversation.

~/.claude.json is the user-level Claude Code config (project
history, MCP servers, IDE state); ~/.claude/settings.json is
editor settings (hooks, permissions, theme). Same product,
two files — Claude Code reads MCP servers ONLY from the
former.

Existing hook + MCP entries from other tools are preserved.
Pass --skip-mcp if you don't want the MCP server registered.

```
aichronicles setup claude-code [flags]
```

### Options

```
      --command string       command to run from each hook (default "aichronicles hook")
  -h, --help                 help for claude-code
      --mcp-command string   command to register as the aichronicles MCP server (default "aichronicles")
      --settings string      path to Claude Code settings.json for HOOKS (default: ~/.claude/settings.json)
      --skip-mcp             do not register the aichronicles MCP server in ~/.claude.json
      --user-config string   path to Claude Code user-config json for MCP SERVERS (default: ~/.claude.json)
```

### SEE ALSO

* [aichronicles setup](./aichronicles_setup.md)	 - Install aichronicles into an AI coding agent or the OS
