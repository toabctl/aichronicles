## aichronicles mcp-serve

Run an MCP server over stdio exposing the user's session history

### Synopsis

Starts a Model Context Protocol server on stdin/stdout that
lets a model query the user's PAST Claude Code / Gemini CLI /
Codex CLI sessions. All tools read the local SQLite store;
nothing writes back.

Tools exposed:
  search_events        — keyword search over past events
  list_sessions        — recent past conversations
  find_episodes        — episodic recall (intent-keyed slices of past sessions)
  get_summary          — cached summary of one session
  list_subagents       — sub-agent threads inside a session
  get_insights         — usage report (top tools / skills / activity)
  list_skills          — installed + invoked skills
  get_skill_staleness  — skills correlated with tool failures
  search_with_summary  — LLM-synthesised answer (requires API key)

Registered automatically by `aichronicles setup claude-code` under
the mcpServers.aichronicles entry of ~/.claude/settings.json.

Logs go to stderr as structured records so the host's MCP log
window surfaces them. Stdin close (client disconnect) ends the
process cleanly.

```
aichronicles mcp-serve [flags]
```

### Options

```
      --db string   SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help        help for mcp-serve
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
