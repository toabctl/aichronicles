# Threat model

This page is a contract, not marketing. It states what aichronicles
promises about the data it sees, what it does **not** promise, and
where the trust boundaries are. Read it before adopting if you
handle anything you'd rather not leak — financial credentials, work
under NDA, anything subject to compliance regimes.

## TL;DR

- aichronicles is **single-user, single-machine** software. Every
  piece of state — the daemon, the SQLite store, the Unix socket,
  your config — runs as your user, on your local disk.
- Secrets matching ~15 detectors are **scrubbed at the edge**
  (before envelopes leave the hook subprocess) and verified at
  the daemon. Egress to the LLM provider also scrubs.
- aichronicles **does not** prevent secrets from landing in
  `~/.claude/projects/*.jsonl`. Claude Code writes those before any
  hook fires; that file tree is out of our control.
- The redactor is **best-effort** — pattern-based, not semantic.
  Anything not matching a detector survives. Run
  `aichronicles audit` periodically.

If those constraints are dealbreakers, don't use aichronicles for
that data.

## Trust boundaries

```
┌─────────────────────────────────────────────────────────────┐
│  YOUR MACHINE — single user, local disk                     │
│                                                             │
│   Claude Code  ──hook──▶  ingest CLI                        │
│                              │ edge redact                  │
│                              ▼                              │
│   MCP client  ◀────stdio────▶  mcp-serve                    │
│                                  │                          │
│   browser    ◀──HTTP/127.0.0.1──▶  aichronicles web         │
│                                  │                          │
│                              ▼   │ read-only                │
│                           SQLite store                      │
│                              ▲   ▲                          │
│                              │   │                          │
│   summarize/reflect/propose ─┘   └─ aichroniclesd ◀──UDS──  │
│        │                            (0600 socket,             │
│        │ egress redact              0700 parent dir)         │
│        ▼                                                    │
└────────┼────────────────────────────────────────────────────┘
         │
         ▼
   Anthropic / OpenAI HTTPS  ◀── only on summarize/reflect/propose
```

`aichronicles web` is a short-lived CLI subprocess (same shape as
`mcp-serve`) that boots a small HTTP server on `127.0.0.1` and
opens the SQLite store read-only. Localhost is the auth boundary
— no authentication, no TLS — mirroring the daemon's choice of
0600 UDS rather than network sockets. Binding to a non-loopback
address is opt-in (`--bind 0.0.0.0`) and surfaces a startup
warning; it explicitly leaves the single-user trust model and is
not a supported posture.

Inside the box: untrusted *between* boundaries (the daemon doesn't
trust the CLI's redaction claim; the egress redactor scrubs again
before talking to a third-party LLM), but unauthenticated *across
UID* — any process running as your UID can read everything
aichronicles writes. We don't defend against an attacker already
on your account.

## What aichronicles promises

### 1. Ingest is the single point of redaction truth

The store has two distinct write paths, each with its own
enforcement layer. Read paths (MCP `tools/call`, the CLI's
`search` / `sessions` / `summaries show`, the `aichronicles web`
server) render content directly without re-scanning. A separate
egress layer protects the read path that *does* leave the local
trust boundary — the LLM call.

**Hook write path** — events:

- **Edge (in `aichronicles ingest`):** Every hook payload is
  scrubbed via `redact.Default()` *before* the envelope is
  serialized for the daemon. Source-of-truth: the patterns in
  `internal/redact/redact.go:builtinDetectors`. Patterns that
  fired land in `envelope.Redaction.Patterns`.
- **Daemon (`aichroniclesd`):** Refuses any envelope where
  `redaction.applied != true`. The daemon does not silently
  re-scrub — that would mask a broken client. It rejects with
  HTTP 400 and logs the source agent.

**LLM-response write path** — `llm_outputs`:

- **Store (`SaveLLMOutput` in `internal/store/llm_outputs.go`):**
  Body is run through `redact.Outbound` before insertion. The
  edge redactor never sees an LLM response — it lands here, not
  via /v1/ingest — so the store is the enforcement point. An
  LLM that hallucinates a credential into its `summarize` reply
  cannot land it in the store unscrubbed.

**LLM-egress to third party** — outbound network:

- **Prompt builders (`prompts.Build*`):** Every user-content
  string routed into an LLM prompt — event content, tool names,
  session digests, URLs — passes through `redact.Outbound()`
  *before* it joins the prompt. This layer protects data
  crossing to a third-party (Anthropic / OpenAI), a different
  trust boundary from the local read paths above.

A fourth scrub happens on **error bodies** from the LLM provider:
the SDK's error message includes the upstream response verbatim,
which would leak your API key if the provider ever echoed it back.
The Anthropic and OpenAI adapters both run `redact.Outbound` over
the error message before returning it.

#### When the detector set changes

Adding a new detector pattern doesn't retroactively scrub rows
that landed before it existed. `aichronicles scrub` is the
operational primitive: it walks the store, runs the current
detector set over every event's `content_text`, and rewrites rows
that match. Run it once after any pattern change — the read paths
then surface the rewritten content unchanged.

The previous design wrapped MCP `tools/call` (and was planned for
the web server) in a second `redact.Outbound` pass on every byte
that left those paths, providing automatic retroactive coverage.
That layer was removed in favour of explicit scrub: cheaper, with
behaviour the operator can observe rather than a silent fix-up
applied at every read.

### 2. Detection coverage

The current detector set, in order of registration (earlier wins on
overlap):

| Pattern              | Catches                                                          |
| -------------------- | ---------------------------------------------------------------- |
| `anthropic_api_key`  | `sk-ant-` + 20+ chars `[A-Za-z0-9_-]`                            |
| `openai_api_key`     | `sk-` (optionally `proj-`) + 40+ chars `[A-Za-z0-9_-]`           |
| `google_api_key`     | `AIza` + 35 chars `[0-9A-Za-z_-]`                                |
| `github_pat_fine_grained` | `github_pat_` + 82 chars `[A-Za-z0-9_]`                     |
| `github_pat_classic` | `gh[pousr]_` + 36 chars `[A-Za-z0-9]`                            |
| `aws_access_key`     | `AKIA` + 16 chars `[0-9A-Z]`                                     |
| `npm_token`          | `npm_` + 36 chars `[A-Za-z0-9]`                                  |
| `slack_token`        | `xox[abprs]-` + numeric + alphanumeric                           |
| `stripe_key`         | `(sk\|pk\|rk)_(live\|test)_` + 24+ chars                         |
| `twilio_sid`         | `(AC\|SK)` + 32 hex chars                                        |
| `pem_private_key`    | full PEM block (BEGIN/END `PRIVATE KEY` headers)                 |
| `jwt`                | three base64url chunks separated by `.`                          |
| `bearer_token`       | `bearer ` (case-insensitive) + 20+ chars                         |
| `db_connection_string` | `postgres://`, `mysql://`, `mongodb://`, `redis://`, `amqp://` with creds |
| `basic_auth_url`     | any `http(s)://user:password@host`                               |
| `aws_secret_key_assignment` | `aws_secret_access_key = ...` style assignments           |

Run `aichronicles audit` to scan the current store against the
current detectors. New rows ingested before a detector existed
won't be retroactively cleaned — use `aichronicles scrub --yes`
(after backing up the DB) to rewrite them in place.

### 3. Local-only data plane

The daemon listens on a Unix-domain socket at
`$XDG_RUNTIME_DIR/aichronicles/sock` with mode `0600`. The parent
directory is `0700`. There is no TCP listener. There is no remote
access path.

The SQLite store lives at `$XDG_STATE_HOME/aichronicles/store.db`
(typically `~/.local/state/aichronicles/`) with mode `0600`. The
daemon process inherits these defaults via the standard Go
`os.MkdirAll(0o700)` + `os.OpenFile(0o600)` calls.

### 4. No outbound network on the ingest path

Capturing a hook event is a strictly local operation. The hook
subprocess connects to the UDS, the daemon writes to the local
SQLite, and that's it. **No network egress happens during ingest.**

Outbound HTTPS only fires when you run `aichronicles summarize`,
`reflect`, or `propose` — and only on cache miss. Once a summary
is cached in `llm_outputs`, replaying it never touches the network.

### 5. Config-file integrity

If `[llm.anthropic].api_key_command` or `[llm.openai].api_key_command`
is set in `~/.config/aichronicles/config.toml`, aichronicles
**refuses to load** the file unless its mode is `0600`. The
rationale: the command is a trust boundary; if anyone else on the
box can rewrite the config, they can redirect the key fetch to an
attacker-controlled command. The mode check fires whenever any
provider has a key command set.

## What aichronicles does NOT promise

This is the load-bearing section. Read it twice.

### 1. `~/.claude/projects/*.jsonl` is out of scope

Claude Code writes its own transcript files to disk **before** any
hook runs. aichronicles consumes the hook events that fire *as a
result* of Claude Code's actions — but it has no opportunity to
intercept the bytes Claude Code writes to its JSONL files.

**Practical consequence:** anything you have ever typed into Claude
Code is in those files, in plaintext, on your disk, regardless of
what aichronicles does. Including:

- API keys you pasted into a prompt to ask Claude how to use them.
- Secrets that escaped your shell into Claude's tool-result blocks.
- Customer data you discussed in Claude.

If you want those files cleaned, you have to clean them yourself.
A `sanitize-claude-transcripts` subcommand that does this in-place
is on the roadmap; until it lands, treat the JSONL tree as a
plaintext archive.

### 2. The redactor is pattern-based and best-effort

We catch ~15 known credential shapes. We don't catch:

- Custom company API keys with no distinctive prefix.
- Personal data (names, addresses, emails, phone numbers) — out of
  scope by design; redacting PII would create more noise than
  signal in your search results.
- Secrets pasted *between* tokens that defeat regex word boundaries
  (e.g. an API key wrapped in unusual delimiters).
- Tokenized JWT payloads that don't fit the `eyJ...` shape.
- Custom certificate chains that don't carry a standard PEM
  `BEGIN`/`END` `PRIVATE KEY` header pair.

**Mitigation:** run `aichronicles audit` periodically. Add new
detectors via PR. The detector list is regenerated into
[reference/redaction-detectors.md](../reference/redaction-detectors.md)
on every release.

### 3. UID-level isolation only

Any process running as your UID can:

- Connect to the daemon's UDS and submit events.
- Read the SQLite store directly with `sqlite3
  ~/.local/state/aichronicles/store.db`.
- Read your `~/.config/aichronicles/config.toml`.
- Read `~/.claude/projects/`.

aichronicles does not authenticate peers on the UDS. We don't
defend against an attacker already on your account. If you've been
told the laptop is compromised, treat the store as compromised.

### 4. No multi-user, no central server, no remote backup

aichronicles is single-user, single-machine. There is no team
mode, no shared corpus, no cloud sync. If you want any of those,
this isn't the project.

### 5. Full-disk encryption is your problem

The store is unencrypted SQLite. Anyone with read access to the
disk file (e.g. someone who steals your laptop without LUKS, or
who rsync's `~/.local/state/aichronicles/`) gets every event
you've captured.

**Recommendation:** run on a LUKS-encrypted root, the way most
serious developer laptops are set up.

### 6. Shell history is your problem

`ANTHROPIC_API_KEY=sk-ant-... aichronicles summarize ...` puts
your key in `~/.local/share/fish/fish_history` (or bash/zsh
equivalent). aichronicles can't help you there. Use the
`api_key_command` config + `secret-tool` workflow instead — no
secrets touch your shell history.

### 7. Outbound LLM calls send your data to a third party

When you run `summarize`/`reflect`/`propose`, aichronicles sends
the relevant event content, session digests, and URL lists to
Anthropic or OpenAI over HTTPS. The egress redactor scrubs known
secrets before transmission, but everything else — your prompts,
file paths, code snippets, tool results — goes to the provider
verbatim.

If your engagement contract or compliance regime forbids sending
session content to a third-party LLM, **don't run those
subcommands**. Capture-only (the daemon + ingest + MCP read paths)
involves no outbound network and is safe under those constraints.

### 8. The MCP server is read-only — but it reads everything

`aichronicles mcp-serve` exposes three tools to whatever client
connects to it (currently Claude Code via stdio). The tools are
strictly read-only — no `INSERT`, no `DELETE`. But they expose:

- Full-text search across every event you've captured (`search_events`).
- The full session list with cwds, timestamps, first prompts (`list_sessions`).
- Any stored summary, reflection, or proposal body (`get_summary`).

The client process that reads from the MCP server has access to
your entire corpus while it's connected. **If you don't trust the
client, don't connect it.** Output is rendered verbatim from the
store; ingest is the single point of redaction truth, and
`aichronicles scrub` is the operational primitive when the
detector set changes (see "Ingest is the single point of redaction
truth" above).

## Recommended hygiene

If you adopt aichronicles, the following are reasonable defaults:

- **Run on a LUKS-encrypted disk.** Stops the laptop-theft path.
- **Audit weekly.** `aichronicles audit | head` — quick eyeball
  pass for stuff the detectors didn't catch but a human would.
- **Re-scrub after detector additions.** New detector PR merges →
  back up the DB, run `aichronicles scrub --yes`. The scrub is
  irreversible, hence the backup.
- **Use `api_key_command` with `secret-tool`.** Never paste a key
  on the command line.
- **Don't paste secrets into Claude Code prompts.** Even one is
  one too many — they end up in `~/.claude/projects` regardless of
  any redaction we do.
- **Periodically review `~/.claude/projects/`.** It grows
  unbounded. Old transcripts are still plaintext archives of
  whatever you typed at the time. Delete or archive aggressively.
- **Don't run `summarize` on sessions whose contents you can't
  legally send to Anthropic / OpenAI.**

## Disclosure

Found a security issue in aichronicles? File an issue at the repo
or contact the author directly. There's no security@ mailbox or
PGP key today; this is a personal toolkit. If the issue is severe
enough that you'd want one, that's signal worth acting on.

## Related

- [Architecture](architecture.md) — how the trust boundaries map
  onto packages and processes.
- [Redaction design](redaction-design.md) — why we scrub
  credentials only and not PII; how detectors are layered.
- [Reference: detector list](../reference/redaction-detectors.md) —
  auto-generated from `builtinDetectors()`.
