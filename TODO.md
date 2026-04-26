# aichronicles TODO

Loose tracking for follow-ups not yet worth their own issue or branch.
Everything here is fair game; pick one off, sketch a plan, ship it.

## Open

### MCP and sub-agent tool_input extraction

Grep / Glob / WebFetch / WebSearch are now first-class — their
tool_input feeds both `content_text` (via
`internal/cli/assemble.go:renderToolContent`) and the typed
`extractions` table (via `pkg/ingest/extract/{Grep,Glob,WebFetch,
WebSearch}Extractor`). Still on the floor: MCP tools (`mcp__*`
namespaced names) and sub-agent invocations (`Task` / Plan / etc.).

Each MCP tool defines its own `tool_input` schema, so a single
hard-coded mapping won't work; we'd need either a per-server
allowlist or a generic "render the most-string-typed value" rule.
For sub-agents, the prompt the agent was launched with is the
high-value field — capturing it would make `aichronicles search`
surface "I delegated work about X to a sub-agent on Tuesday"
without the user having to remember the agent name.

Scope:

- Decide on the rendering shape: per-MCP-server allow-list keyed
  off the `mcp__<server>__<tool>` prefix, or a generic fallback
  that picks the longest string value from `tool_input`. The
  generic fallback is faster to ship and less brittle.
- Extend `internal/cli/assemble.go:toolDetail` to handle the
  generic case for any unknown tool name.
- Add a `SubagentPromptExtractor` in `pkg/ingest/extract/` that
  emits `kind=subagent_prompt` from `Task`-shaped tool_input
  (typically a `prompt` or `description` field).
- Cover with unit tests parallel to `TestRenderToolContent` and
  the existing per-tool extractor tests.
