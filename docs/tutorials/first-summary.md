# Your first summary

By the end of this page (≈3 minutes) you'll have an Anthropic API
key wired into aichronicles, generated a structured summary of one
of your sessions, and seen what the rendered output looks like.

This page assumes you've completed [Get
started](getting-started.md). If `aichronicles sessions` returns
rows, you're ready.

## 1. Get an API key

aichronicles supports two providers:

- [Anthropic](https://console.anthropic.com/settings/keys) — what
  Claude Code itself uses. Default in this tutorial.
- [OpenAI](https://platform.openai.com/api-keys) — see [Switch
  providers](../how-to/switch-providers.md).

Create a key in either console. **Don't paste it on the command
line** — that puts it in your shell history. We'll stash it in your
desktop keyring.

## 2. Stash the key in your keyring (≈30s)

```fish
secret-tool store --label='Anthropic API' service anthropic user default
```

`secret-tool` prompts for the value on stdin — paste your key, press
Enter. It writes to whatever Secret-Service backend your desktop
runs (GNOME Keyring, KWallet, KeePassXC). Verify:

```fish
secret-tool lookup service anthropic user default | wc -c
# 109   (or thereabouts; ~108-byte key + newline)
```

If `wc` prints `0`, the store failed. Re-run the `secret-tool
store` line.

## 3. Tell aichronicles how to fetch the key (≈30s)

Create the config file at `~/.config/aichronicles/config.toml`:

```fish
mkdir -p -m 0700 ~/.config/aichronicles
cat > ~/.config/aichronicles/config.toml <<'EOF'
[llm]
provider = "anthropic"

[llm.anthropic]
api_key_command = "secret-tool lookup service anthropic user default"
EOF
chmod 0600 ~/.config/aichronicles/config.toml
```

The `0600` mode is mandatory whenever any provider has an
`api_key_command` set — aichronicles refuses to load the config
otherwise. The reason: the command is a trust boundary; if anyone
else on the box can rewrite this file, they can redirect the key
fetch to an attacker-controlled command.

Verify the config loads:

```fish
aichronicles sessions --limit 0
# (no rows; the point is that no error is printed)
```

## 4. Pick a session to summarize (≈10s)

```fish
aichronicles sessions --since 24h --limit 5
```

Each row starts with an 8-char session id prefix. Pick one — the
one you want a summary of. You don't need the full UUID;
`aichronicles summarize --session <prefix>` resolves it to the full
id automatically.

## 5. Generate the summary (≈30-60s)

```fish
aichronicles summarize --session <prefix>
```

What happens:

1. The session's events load from SQLite.
2. Pre-extracted URLs from the session join the prompt as
   "Links observed", to be annotated by the LLM.
3. The full prompt + tool definition are hashed; if a row with that
   `prompt_hash` already exists in `llm_outputs`, you get a cache
   hit and no API call is made.
4. On a cache miss, your API key is fetched fresh from the keyring,
   the call is made, the model returns a structured tool_use
   payload, and the JSON body is stored. Subsequent runs hit the
   cache.

The first call on a long session can take 10-30 seconds with no
intermediate output — it's not stuck, it's waiting for the model.

## What you get back

The rendered output looks like this:

```text
Topic: feat(grass): prefer stereo version_data over GitHub tags

What was done:
  - Added stereo.go with YAML structs for version_data/*.yaml
  - Extended grass.Analyze with WithStereoDir option
  - Wired the option through manifest-gen + cg agent grass CLI

Unresolved:
  - cg-codeowners-check pending — awaiting human review

Key files:
  - agents/pkg/agents/grass/stereo.go
  - bots/manifest-gen/internal/agent/analyzer/analyzer.go

Links:
  - https://github.com/chainguard-dev/customer-issues/issues/3406
    source ticket for the harbor cert work
```

The same content is stored as JSON. To see the raw form (for
piping into `jq`, for example):

```fish
aichronicles summarize --session <prefix> --json
```

This is a cache hit on the second call — no API charge.

## What's next

- [**Inspect stored summaries**](../how-to/inspect-stored-summaries.md) — `aichronicles summaries list` and `summaries show` for browsing the corpus.
- [**Reflect across sessions**](../how-to/reflect-across-sessions.md) — `aichronicles reflect --since 7d` for patterns across a week.
- [**Register with Claude Code via MCP**](../how-to/register-with-claude-code.md) — make summaries readable by the agent itself.
- [**Switch providers**](../how-to/switch-providers.md) — same flow with OpenAI.

## If something broke

- **"`api_key_command` is set; refuse to trust a world/group-
  accessible config"** — your config file isn't `0600`. Run `chmod
  600 ~/.config/aichronicles/config.toml`.
- **"`api_key_command` produced empty output"** — `secret-tool
  lookup` returned nothing. Either the entry doesn't exist (re-run
  step 2) or the keyring is locked (unlock it via your desktop's
  keyring UI).
- **HTTP 401 / "invalid x-api-key"** — the key in your keyring is
  wrong. Generate a new one at the provider console, re-stash, retry.
  (And if the key has been seen anywhere it shouldn't have been —
  shell history, transcripts, this conversation — revoke and rotate
  before debugging anything else.)
- **Cache always hits even though I want a fresh call** — pass
  `--force`. That bypasses the cache and re-calls the LLM,
  inserting a new row alongside the old one (deduped on prompt
  hash, so identical inputs don't double-bill anyway).
