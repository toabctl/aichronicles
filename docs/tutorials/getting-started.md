# Get started

By the end of this page (≈90 seconds) you'll have aichronicles
installed, the daemon running, and your first Claude Code hook event
captured in the local store. No API key required for this page — we
add the LLM-using flow in [Your first summary](first-summary.md).

## Prerequisites

- Linux with **systemd --user** (the daemon ships as a user unit).
- **Go 1.26+** for building from source.
- **Claude Code** installed and configured.
- A shell. Examples below use `fish`; bash/zsh equivalents are
  obvious.

## 1. Install (≈15s)

```fish
go install github.com/toabctl/aichronicles/cmd/aichronicles@latest
go install github.com/toabctl/aichronicles/cmd/aichroniclesd@latest
ln -sf ~/go/bin/aichronicles  ~/.local/bin/aichronicles
ln -sf ~/go/bin/aichroniclesd ~/.local/bin/aichroniclesd
```

The systemd unit expects `aichroniclesd` at `~/.local/bin/`, hence
the symlink. Confirm both are on `$PATH`:

```fish
which aichronicles aichroniclesd
# /home/you/.local/bin/aichronicles
# /home/you/.local/bin/aichroniclesd
```

## 2. Bring up the daemon (≈15s)

```fish
aichronicles setup systemd
systemctl --user start aichronicles.socket
```

What happened: `setup systemd` wrote two unit files
(`aichronicles.socket`, `aichronicles.service`) to
`~/.config/systemd/user/`, told systemd to reload, and enabled the
socket so it starts on every login. Starting the socket gets the
listener up; the service starts on demand the first time anything
connects.

Checkpoint:

```fish
systemctl --user is-active aichronicles.socket
# active

curl --silent --unix-socket /run/user/(id -u)/aichronicles/sock \
     http://unix/v1/healthz
# {"status":"ok"}
```

## 3. Wire Claude Code hooks (≈15s)

```fish
aichronicles setup claude-code --yes
```

This merges six hook entries into your `~/.claude/settings.json`
(`UserPromptSubmit`, `PostToolUse`, `Stop`, `SessionStart`,
`SessionEnd`, `PostToolUseFailure`). Each hook runs `aichronicles
ingest` as a subprocess, which pipes the hook payload through the
edge redactor and POSTs the envelope to the daemon over the UDS.

The hook never fails Claude Code: ingest exits 0 even if the daemon
is unreachable. Outages fire one desktop notification per outage so
you find out without it blocking your session.

## 4. Verify capture (≈15s)

Open Claude Code and type any prompt. As soon as the
`UserPromptSubmit` hook fires, the daemon receives an envelope and
stores it.

```fish
aichronicles sessions --limit 1
# 1f5dd444  2026-04-25T08:21:07Z  2026-04-25T08:21:09Z  3  /home/you  hello world
```

You should see one row per session you've started, newest first.
The columns are:
`session-prefix · started_at · ended_at · event_count · cwd · first
prompt`.

Want to see the events themselves?

```fish
aichronicles search 'hello'
```

This is FTS5 — query syntax follows
[SQLite FTS5](https://www.sqlite.org/fts5.html#full_text_query_syntax).

## You're done

You now have a corpus that grows automatically as you use Claude
Code. Every prompt, tool call, and response is in SQLite at
`~/.local/state/aichronicles/store.db`, with secrets scrubbed at
ingest time.

## What's next

- [**Your first summary**](first-summary.md) — wire an API key and
  generate a structured summary of one session.
- [**Backfill historical sessions**](../how-to/backfill-historical-sessions.md) —
  import your existing `~/.claude/projects/*.jsonl` transcripts in
  one shot.
- [**Register the MCP server with Claude Code**](../how-to/register-with-claude-code.md) —
  let the agent search your past sessions mid-conversation.
- [**The threat model**](../explanation/threat-model.md) — what
  aichronicles promises and what it doesn't.

## If something broke

- **`systemctl --user is-active aichronicles.socket` returns
  `inactive`** — `journalctl --user -u aichronicles.socket -n 30`
  usually has the cause. Most often: `~/.local/bin/aichroniclesd`
  isn't where the unit expects it. Check the symlink.
- **`aichronicles sessions` returns no rows after triggering a
  hook** — the hook is silently failing. Look for ingest errors in
  Claude Code's debug output, or check the daemon's logs:
  `journalctl --user -u aichronicles.service -n 30`.
- **A red desktop notification "aichronicles: daemon unreachable"** —
  the daemon stopped. `systemctl --user restart aichronicles.service`
  and check the journal.
- **Anything else** — see
  [troubleshooting](../how-to/troubleshooting.md) (TODO).
