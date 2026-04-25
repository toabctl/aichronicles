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

### Richer content_text and extractions for non-Bash/file tools

For `tool_use` events, `extractContentText` in
`internal/cli/assemble.go:117-121` returns just the bare tool name
(e.g. `"Bash"`, `"Read"`). Two extractors then pull `tool_input`
into the typed `extractions` table: `ShellCommandExtractor` (Bash
→ `shell_command`) and `FilePathExtractor` (Read/Write/Edit/
NotebookEdit → `file_path`). Everything else — Grep, Glob,
WebFetch, WebSearch, MCP tools, sub-agent tools — drops the
`tool_input` payload on the floor. After migration 006 added the
extractions FTS tier, queries can now find Bash command bodies and
file paths via the typed-fact fallback, but the Grep pattern,
Glob pattern, fetched URL, etc. remain unsearchable.

Two complementary fixes:

- Extend `extractContentText` in `internal/cli/assemble.go` to
  produce a tool-specific one-liner for the common tools
  (`Grep pattern=...`, `WebFetch <url>`, `Glob <pattern>`),
  capped at a sane length. Goes through the existing redaction
  path automatically.
- Add an extractor per tool in `pkg/ingest/extract/extract.go`:
  `WebFetch` → `kind=url`, reusing the existing URL extraction
  contract; `Grep` / `Glob` → a new `kind=pattern`. Mirror
  `FilePathExtractor`'s `tool_name` whitelist pattern so the
  scope stays explicit.

Out of scope (rejected during the search-improvements batch):
adding `tool_name` as a second indexed FTS5 column in `events_fts`.
Tool name is already mirrored into `content_text`, so a multi-
column index would weight the same signal twice without adding
recall.
