# aichronicles TODO

Loose tracking for follow-ups not yet worth their own issue or branch.
Everything here is fair game; pick one off, sketch a plan, ship it.

## Open

### `doctor` should verify agent hooks, not just the daemon

`doctor` probes the daemon, the cron timers and pipeline staleness.
It never checks the thing at the very front of the pipe: whether
any agent is actually wired to call us. Two silent failure modes it
would catch —

- hooks were never installed (or a teardown removed them) for an
  agent the user still uses;
- the hook command is registered but unresolvable — `aichronicles`
  not on the login shell's PATH, or a stale build that rejects the
  `--agent` slug in the entry.

Codex sharpens this. Verified against codex-cli 0.149.1: a hook that
writes to stderr and exits 0 produces no output in the terminal and
nothing in the rollout — Codex prints `hook: SessionStart Completed`
either way. So a wholly broken hook looks exactly like a working
one, and the only symptom is an empty corpus. Codex's trust gate is
a third variant of the same mode: hooks installed, binary fine, but
never armed because the prompt was dismissed.

Cheap version: for each agent whose settings file has our entries,
resolve the command's argv[0] via the login shell and report
WARN on a miss. Reading `[hooks.state]` from `~/.codex/config.toml`
would additionally catch untrusted-but-installed.

### `import-codex` — backfill Codex CLI rollout files

Codex support currently captures live only. History starts the
moment `setup codex-cli` runs; everything already in
`$CODEX_HOME/sessions/<yyyy>/<mm>/<dd>/rollout-*.jsonl` stays
invisible, unlike claude-code and gemini-cli which both have
importers.

The format is a JSONL of `{timestamp, ordinal, type, payload}`
lines where `type` is one of `session_meta`, `turn_context`,
`world_state`, `event_msg`, `response_item`, and `response_item`
payloads are OpenAI Responses API items (`message` with
`role: user|assistant|developer`, `custom_tool_call`,
`custom_tool_call_output`, …). `session_meta.payload.session_id`
is the same UUID the hooks report, so imported rows would join
cleanly onto hook-captured ones.

Deliberately deferred: the one thing that makes an importer safe
is broad coverage of the item shapes, and the shapes not seen in a
plain shell-and-edit session (reasoning items, `function_call`,
`local_shell_call`, compaction boundaries, `code_mode` `exec`
calls carrying JS) would be guesswork. Worth doing against a real
corpus of varied rollouts, not one sample.

## Done

### Surface LLM token usage and rough cost — shipped 2026-04-28

`aichronicles usage` (CLI) and `/usage` (web) aggregate
`llm_outputs.input_tokens` / `output_tokens` by day × kind × model.
Optional cost estimation reads `$XDG_CONFIG_HOME/aichronicles/prices.toml`
(schema: `[models."<name>"] input_per_mtok, output_per_mtok`); no file
means no Cost column rather than a fabricated number. JSON output via
`--format=json` for jq pipelines. MCP tool intentionally not shipped
in v1 per the original scoping note. Per-conversation cost attribution
remains out of scope (tokens are recorded per llm_output, not per event).
