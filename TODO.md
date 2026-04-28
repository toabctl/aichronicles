# aichronicles TODO

Loose tracking for follow-ups not yet worth their own issue or branch.
Everything here is fair game; pick one off, sketch a plan, ship it.

## Open

(empty)

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
