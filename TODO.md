# aichronicles TODO

Loose tracking for follow-ups not yet worth their own issue or branch.
Everything here is fair game; pick one off, sketch a plan, ship it.

## Open

### Surface LLM token usage and rough cost

Per-row token counts are already persisted —
`llm_outputs.input_tokens` and `output_tokens` (see
`internal/store/llm_outputs.go:24-34`) get filled from
`resp.Usage` in every summarize / reflect / propose call. What's
missing is a user-facing way to see the totals: today the only
way to ask "how many tokens did I burn last week?" is to drop
to SQL.

Scope:

- New CLI command `aichronicles tokens` (or `usage`) that
  aggregates `llm_outputs` rows. Default view: per-day totals
  for the last 30 days, broken down by `kind` (summary /
  reflect / propose) and `model`. `--since 7d` / `--month`
  flags for windows; `--format=json` for jq.
- Optional cost estimation. The provider name is in
  `llm_outputs.model`; ccusage maintains a curated price table
  we could borrow (it pulls from LiteLLM's pricing dataset). Or
  start with a simple TOML config under
  `~/.config/aichronicles/prices.toml` listing $/Mtok per model
  — explicit, no network at runtime, easy to override when
  Anthropic changes prices.
- Web UI: a `/usage` page using the same aggregation, pico-
  styled table. Same shape as the sessions page.
- MCP tool? Probably not v1 — agents asking "how much have I
  cost the user this week" is a niche use case. Surface via CLI
  / web first, add MCP later if it earns it.
- Out of scope: per-conversation cost tracking. Would need a
  way to attribute prompt tokens to specific events (which we
  don't have today; tokens are recorded per-llm-output, not
  per-event). ccusage solves a different problem (parsing
  Claude Code's JSONL for cost) and is the right tool when you
  want that view.
