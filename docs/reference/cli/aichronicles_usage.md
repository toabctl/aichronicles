## aichronicles usage

Per-day LLM token totals (input/output) by kind and model

### Synopsis

Aggregates llm_outputs.input_tokens / output_tokens by
day × kind × model so you can answer 'how many tokens did I
burn last week, and on what?' without dropping to SQL.

Optional cost estimation: drop a TOML at
$XDG_CONFIG_HOME/aichronicles/prices.toml with the per-Mtok
rates for your models and the table grows a COST column. No
file = no cost column (aichronicles ships no built-in price
list — vendor prices change too often to bake in).

Schema for prices.toml:

  [models."claude-sonnet-4-6"]
  input_per_mtok  = 3.00
  output_per_mtok = 15.00

--format=json emits the rows + totals as a structured
payload suitable for jq.

Talks to aichronicles-api over its UDS (override with
--socket or $AICHRONICLES_API_SOCKET).

```
aichronicles usage [flags]
```

### Options

```
      --format string    output format: table (human-readable) or json (for jq / scripts) (default "table")
  -h, --help             help for usage
      --prices string    path to prices.toml (default: $XDG_CONFIG_HOME/aichronicles/prices.toml)
      --since duration   only consider llm_outputs within this window (e.g. 7d, 30d, 24h) (default 720h0m0s)
      --socket string    aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
