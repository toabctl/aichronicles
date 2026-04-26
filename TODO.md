# aichronicles TODO

Loose tracking for follow-ups not yet worth their own issue or branch.
Everything here is fair game; pick one off, sketch a plan, ship it.

## Open

### Add `LICENSE` (Apache-2.0)

The README references one but the file isn't committed yet. Drop in
the canonical Apache-2.0 text (e.g. via `gh repo create --license=
apache-2.0` or by copying from
https://www.apache.org/licenses/LICENSE-2.0.txt) before the first
public-release commit. Confirm the README's License section matches.

### Shell completion for `--session` (and the `summaries show` arg)

Today users type 8-char prefixes and `store.ResolveSessionIDPrefix`
expands them. The next step is shell completion: as soon as the user
starts typing a session id (or prefix), tab cycles through matching
sessions from the live store.

Scope:

- Add a `cobra.Command.ValidArgsFunction` (for `summaries show <session>`)
  and `cmd.RegisterFlagCompletionFunc("session", ...)` on the three
  subcommands that take `--session`: `summarize`, `search`, `summaries
  list`.
- Each completion func opens the store read-only, queries
  `SELECT id FROM sessions WHERE id LIKE ? || '%' ORDER BY ...`,
  and returns the matching ids (full uuid, not the 8-char preview —
  the shell handles partial matching from the user's current input).
  Cap to ~50 candidates so a blank tab doesn't list thousands.
- Surface the cwd and the first user prompt as the completion
  description (cobra's `cobra.ShellCompDirective` + tab-separated
  `id\tdescription`). Makes "which session was the chainguard one?"
  obvious without a second command.
- Wire `aichronicles completion <bash|zsh|fish>` (cobra has it built-in
  via `cmd.GenFishCompletion` etc.) — or rely on cobra's default
  `completion` subcommand which Execute() auto-installs.
- Document the install path in `docs/getting-started.md` once docs land.

Implementation hint: opening the store inside a completion func is
fine — it's read-only, the daemon's WAL handles concurrency, and tab
completion is interactive (a 50ms query is invisible).

### Codex CLI support (OpenAI's `codex`)

OpenAI's Codex CLI ships a hook system whose shape lines up cleanly
with ours: stdin JSON per event, exit 0 = continue / exit 2 = block,
events `PreToolUse` / `PostToolUse` / `UserPromptSubmit` / `Stop`,
config in `~/.codex/hooks.json` or `<repo>/.codex/hooks.json` with the
same `{matcher, hooks: [{type: "command", command: "..."}]}` layout.
Codex hooks must be enabled with `[features] codex_hooks = true` in
`config.toml`.

The envelope already anticipates this — `source_agent` is a slug field
and the OpenAPI lists Codex-class agents as intended sources — so the
work is glue, not redesign.

Scope:

- Adapter in `internal/ingest/` (or inside the `ingest` CLI dispatch)
  that recognizes Codex's stdin JSON and maps it to an `Envelope` with
  `source_agent: "codex"`. Field-by-field mapping for `tool_name` /
  `tool_input` / `hook_event_name` / `session_id` etc. Some fields
  differ from Claude Code's payload shape — pin them with golden-file
  tests parallel to `import_claude_test.go`.
- `aichronicles setup codex-cli` subcommand mirroring `setup
  claude-code` (`internal/cli/setup.go`). The current
  `installedHooks` constant becomes per-agent. Idempotently merge our
  command into `~/.codex/hooks.json`, preserving any existing entries
  from other tools, exactly like the Claude Code installer does.
- Matching `teardown codex-cli`.
- Docs note: instruct users to flip `codex_hooks = true` in
  `~/.codex/config.toml` (we shouldn't rewrite their TOML for them).
- Redaction invariant and UDS transport are unchanged — both are
  agent-neutral.

References:
- https://developers.openai.com/codex/hooks
- https://developers.openai.com/codex/config-advanced

### First-class subagent threads

Claude Code's `SubagentStart` / `SubagentStop` already round-trip through
`internal/cli/assemble.go:29-30` as `subagent_start` / `subagent_stop`
event kinds, but they're flat — there's no parent/child linkage, no way
to ask "what did the planner subagent do," and summaries don't
distinguish a tool call run by the main agent from one run inside a
subagent. Peer tools (e.g. disler/claude-code-hooks-multi-agent-
observability) surface this as swim lanes; we should at least make it
queryable.

Scope:

- Pull `agent_id` and `agent_type` from the hook payload in
  `Assemble()` and stash them on the envelope. Likely shape: a new
  optional `Subagent { ID, Type, ParentID }` struct on `ingest.Envelope`,
  serialized under `subagent` in the JSON; absent for top-level events.
- Schema migration `004_subagent_threads.sql`: add nullable
  `subagent_id`, `subagent_type`, `parent_event_id` columns to `events`
  with an index on `(session_id, subagent_id, ts_source_ms)`. Populate
  on ingest from the new envelope fields. Older rows keep NULLs.
- The store-side projection in `internal/store/ingest.go` needs to
  match envelope start events to their stop events (by `agent_id`
  within a session) so a subagent's lifetime is queryable as a span,
  not just two point events.
- MCP `search_events` grows an optional `subagent_id` filter; a new
  `list_subagents` tool returns `(session_id, subagent_id, type,
  started_at, ended_at, event_count)` rows so an agent can ask "what
  did my planner do last Tuesday."
- Prompt builders in `pkg/llm/prompts/prompts.go` should label each
  event with its subagent (e.g. `[planner] tool_use Read ...`) so the
  summary tool-call output can attribute work to the right thread.
  Add a `subagents` field to `SummaryResult` listing the threads that
  ran and what each did.
- Extend `aichronicles sessions` output to show subagent count next
  to event count when nonzero. Cheap, makes the structure visible.
- Tests: golden-file an envelope set with a planner + worker
  subagent and assert the projection links them correctly. Match the
  shape of `import_claude_test.go`.

Out of scope (for this entry): live HITL response routing — the
observability project ships an interactive permission dialog flow,
but that's an interactive UI feature and aichronicles is a
read/capture tool, not an agent host.

### Drop egress redaction (read-path layer only)

`redact.Outbound` currently runs on three different egress paths:
the MCP `tools/call` response in `internal/mcp/mcp.go`, the prompt
builders in `pkg/llm/prompts/prompts.go`, and the SDK error
scrubbers (`scrubAnthropicError` / `scrubOpenAIError` in
`pkg/llm/`). The defense-in-depth rationale was: a detector
added after a row was stored still scrubs the old row when read
out.

That benefit is small in practice. aichronicles ships
`aichronicles scrub` (`internal/cli/scrub.go`) precisely for the
"I added a new detector, rewrite stored rows" case, and ingress
redaction enforced at four layers (CLI edge, daemon, store, and
the OpenAPI invariant) is the actual single point of truth.
Running the detector set on every byte that leaves wastes CPU,
adds a second site for detector-set bugs to live, and complicates
new read paths (the upcoming `aichronicles web` server is the
immediate trigger for this cleanup).

Operationally: add detector → run `scrub` → reads are safe.
That's a clearer story than "redact twice; the second time is a
silent fix-up nobody can observe."

Scope:

- Drop the `redact.Outbound` calls from `internal/mcp/mcp.go`'s
  `handleToolsCall` egress wrapper and any per-handler scrubs.
  Tests that assert "egress scrubs a planted secret" become
  "ingest refuses an envelope carrying that secret"
  (`TestToolsCall_ScrubsEgressText` in
  `internal/mcp/tools_aichronicles_test.go` is the relevant one
  to rewrite).
- Update `docs/explanation/threat-model.md` to drop boundary 4
  ("egress redaction") from the diagram and explain the new
  posture: ingress is the single point of truth; `scrub` is the
  operational tool when the detector set changes.
- The new `aichronicles web` server (planned) does NOT build a
  redact-on-write response wrapper. Reads render `content_text`
  directly.

Decide separately, not part of this cleanup: the prompt-builder
layer in `pkg/llm/prompts/prompts.go` and the SDK error scrubbers.
Those scrub content before it leaves the local trust boundary to
a third-party LLM provider — a different threat model from
"prevent local read paths from re-emitting a credential." Likely
keep them; document the asymmetry in the threat-model so the
reasoning is captured.

Non-goal: removing `aichronicles scrub`. It stays as the
canonical path for "I changed the detector set, refresh stored
rows."

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
