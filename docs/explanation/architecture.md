# Architecture

The map you read when you're about to change code, or when something
doesn't make sense and you want to understand why it's shaped the
way it is. Complements [data-flow.md](data-flow.md) (the dynamic
view) with the static view: what packages exist, what depends on
what, what the schema looks like, and which design decisions are
load-bearing.

For the high-level system overview (component diagram), see the
[README](../../README.md). It's the canonical "what are the moving
parts" — not duplicated here so the two can't drift.

## Layered architecture

aichronicles is organised as a hexagonal / ports-and-adapters
shape: a public domain core (`pkg/events`) at the center,
infrastructure adapters around it (`internal/store` for SQLite,
`internal/daemon` for HTTP, `internal/web`, `internal/mcp`),
and application orchestration on top (`internal/cli`).

```
                ┌────────────────────────────────────────┐
 Entry points   │ cmd/aichronicles    cmd/aichroniclesd  │
                └────────┬─────────────────────┬─────────┘
                         │                     │
                ┌────────▼─────┐       ┌───────▼────────┐
 Application    │ internal/cli │       │ internal/daemon│
 (orchestration)│ (~50 files)  │       │ (HTTP + UDS)   │
                └────┬───────┬─┘       └────────┬───────┘
                     │       │                  │
              ┌──────▼─┐ ┌───▼──────┐  ┌────────▼──────┐
 Adapters     │internal│ │internal/ │  │ internal/web  │
              │/agents │ │   mcp    │  │   (HTML+SSE)  │
              └────────┘ └──────────┘  └───────────────┘
                              │   │           │
                              └───┴───────────┘
                                      │
                              ┌───────▼──────────┐
 Storage adapter              │  internal/store  │  ← SQLite-bound
 (SQL implementation)         │   monolithic;    │     events.Sink
                              │   ~25 files      │     lives here
                              └───────┬──────────┘
                                      │
                                      ▼
                              ┌──────────────────┐
 Domain core                  │   pkg/events     │  ← public, no SQL,
 (event model + pipeline)     │                  │     no HTTP, no I/O
                              │  envelope, kinds,│
                              │  views, episode, │  ← types
                              │  redact, role,   │
                              │  nullable        │
                              │                  │
                              │  source, sink,   │  ← interfaces
                              │  extractor,      │
                              │  pipeline        │
                              │                  │
                              │  sources/        │  ← concrete sources
                              │    claude/       │
                              │    gemini/       │
                              └──────────────────┘
```

## Process model

Two binaries, plus systemd timers for periodic work.

| Binary | Process kind | Lifecycle | Owns |
|---|---|---|---|
| `aichroniclesd` | Long-running daemon | systemd `--user` unit, socket-activated | The HTTP ingest API. **Only this.** |
| `aichronicles ingest` | Short-lived hook subprocess | Forked per Claude Code / Gemini CLI hook event | One envelope's worth of translation, edge redaction, and POST to the daemon |
| `aichronicles induction sweep` | Short-lived periodic | `aichronicles-cron-induction.timer` (15min default) | Single-session induction across idle sessions |
| `aichronicles meta sweep` | Short-lived periodic | `aichronicles-cron-meta-analysis.timer` (1h poll, per-kind cadences in SQLite) | Cadence-gated meta-analyses (propose / reflect / challenge / digest_weekly / skill_revision) |
| `aichronicles digest weekly` | Short-lived periodic | `aichronicles-cron-weekly-digest.timer` (`OnCalendar=Mon 06:00:00`) | Weekly retrospective digest |
| `aichronicles <other>` | Short-lived user CLI | Forked per command | Read paths, imports, summarize, MCP server (`mcp serve`), web (`web serve`) |

Why this split:

- **The daemon owns the write path** to SQLite. Centralising
  writes in one process means we don't need cross-process
  coordination for the `ingest_seq` counter or the FTS5 trigger
  machinery — SQLite's WAL handles concurrency, but the daemon
  serialises the writes that matter.
- **The ingest CLI is fire-and-forget by design.** A wedged daemon
  can never block a hook; the CLI exits 0 even on POST failures,
  logging structured warnings to stderr.
- **Periodic work used to live in the daemon as goroutines.** It
  doesn't anymore — sweepers are systemd-timer-driven CLI
  subcommands. Failure isolation, per-run journal logs, and
  suspend catch-up via `Persistent=true` are properties the
  in-process ticker couldn't give.
- **The other CLIs are read-mostly.** They open the same SQLite
  file (with a few exceptions like `import-*`, `scrub`,
  `summarize`'s `llm_outputs` write). WAL mode tolerates concurrent
  readers during writes.

## Package map

```
cmd/
  aichronicles/         CLI multiplexer entrypoint
  aichroniclesd/        daemon entrypoint (UDS HTTP server only)

pkg/                    public, no stability promise
  events/               domain core
    envelope.go         Envelope, Tool, Subagent, Redaction, Ack,
                        Validate, ValidationError, ErrInvalid,
                        DeriveSessionID, agentSlugPattern
    kinds.go            Kind* / Role* enum + IsValidKind / IsValidRole
    nullable.go         NullString / NullInt64 (sql-free)
    redact.go           Redactor interface + ScannerRedactor +
                        ApplyRedaction free function
    role.go             RoleForKind helper
    tool_rendering.go   RenderToolContent + tool-input field-mapping
                        (shared between Claude and Gemini hooks)
    views.go            EventView (read shape for prompt builders)
    episode.go          Episode (episodic-memory unit)
    event.go            Event{Envelope, Raw, Extractions},
                        Result, Stats, ErrRedactionRequired
    extractor.go        Extractor func type, Extraction value,
                        ExtractorRegistry, ExtractionKind*
                        constants, DefaultExtractors()
    extractors_builtin.go   URL / Bash / FilePath / WebFetch / Skill
    source.go           Source interface (iter.Seq2 stream)
    sink.go             Sink interface + SinkStats
    pipeline.go         Pipeline value type, Run + Process methods
    sources/            concrete Source implementations
      claude/
        hook.go         HookTranslator (single-shot)
        jsonl.go        JSONLSource (streaming)
      gemini/
        hook.go         HookTranslator
        transcript.go   TranscriptSource

  redact/               detector library + scanner combinators
                        (no aichronicles-internal deps)
  llm/                  provider-neutral interface
    config.go           Provider / Config / FromConfig switchboard
    anthropic.go        adapter using anthropic-sdk-go
    openai.go           adapter using openai-go
    prompts/            BuildSummary / BuildReflect / BuildPropose

  ingest/               wire schema (legacy alias kept for backward
                        import paths; canonical home is pkg/events
                        — see "events vs ingest" below if you find
                        the alias still in use anywhere)

internal/               private; only this binary imports
  agents/               Claude Code / Gemini CLI integration metadata
                        (slug, hook event names, settings.json paths
                        — consumed only by `setup` / `teardown`)
  cli/                  cobra subcommands; ~50 files
    ingest.go           hook subprocess body (translates via
                        pkg/events/sources/{claude,gemini}.HookTranslator)
    setup*.go,          systemd + agent-hook wiring
    teardown*.go
    import_claude.go,   backfill paths — each builds an
    import_gemini.go,   events.Pipeline + events.Source +
    import_jsonl.go     store.BufferedSink and calls Pipeline.Run
    summarize.go,       LLM-using subcommands; consume
    reflect.go,         events.EventView etc. via store loaders
    propose*.go,
    induction.go,
    facts.go,
    digest.go,
    insights.go,
    skills_*.go
    audit.go, scrub.go  redaction inspection
    search.go           FTS5 search
    sessions.go         session listing
    mcp_serve.go        MCP host (cobra wrapper around internal/mcp)
    meta_sweep.go       cadence-gated meta-analyses dispatcher
    assets/             embedded systemd unit files

  daemon/               HTTP server, UDS listener, watchdog
    server.go           Server holds events.Pipeline, handler shrinks
                        to read body → Validate → Pipeline.Process
    systemd.go          notify-socket integration, socket activation
    watchdog.go         WATCHDOG_USEC handling

  store/                SQLite layer; ~25 files
    store.go            Open, Close
    sink.go             events.Sink implementations: Sink (single-tx)
                        and BufferedSink (chunked + row-by-row fallback)
    ingest.go           IngestEnvelope + IngestEnvelopeWithExtractions
    events.go           LoadEvents* (returns []events.EventView),
                        session-completion, FTS read paths
    episodes.go         SegmentSession (operates on events.EventView),
                        SaveEpisodes, FindEpisodes
    sessions.go         session aggregates
    session_links.go    cross-session links (Zettelkasten-style)
    session_outcomes.go session outcome tracking
    skill_*.go          skill staleness, candidates, failures, outcomes
    facts.go            semantic facts
    induction.go        induction outputs
    insights.go         aggregations
    llm_outputs.go      LLM output cache
    search.go           FTS5 query builder
    token_usage.go, prune.go, unresolved.go, projects.go
    migrations/         embedded *.sql, ordered by NNN_ prefix
    migrate.go          migration runner

  mcp/                  MCP JSON-RPC server + tools_aichronicles
  web/                  HTML browser + SSE live activity
  config/               TOML config (Notifications, Capture, LLM,
                        Induction, MetaAnalysis, Limits)
  paths/                XDG resolution
  notify/               freedesktop notifications via dbus
  nullable/             sql.NullString → render-string helpers
                        (read-side only; write-side wrappers stay
                        in store/events.go)
  preview/              one-line snippet helper (CLI / web / MCP)
  pricing/              token-cost calculation per model
  searchquery/          search-query parser
  skills/               skill discovery from ~/.claude/skills
  timefmt/              time formatting

api/
  openapi.yaml          authoritative spec for /v1/ingest, /v1/healthz

integration/            //go:build integration tests
```

### Non-obvious choices

- **`pkg/events` is the domain core.** It imports nothing from
  `internal/*`, no `database/sql`, no `net/http`. It owns the event
  model end-to-end: shape, validation, pipeline, sources, sinks,
  extractors, and the read-side primitives (`EventView`, `Episode`).
  The package has no stability promise but is intentionally
  embeddable.

- **Sources are sub-packaged by agent.** `pkg/events/sources/claude`
  and `pkg/events/sources/gemini` each export a `HookTranslator`
  (single-shot, used by `aichronicles ingest`) and a `JSONLSource`
  / `TranscriptSource` (streaming, used by importers). Adding a
  third agent = one new sub-package; nothing in `pkg/events`
  changes.

- **The Sink interface lives in `pkg/events`; the SQLite implementation
  lives in `internal/store`.** Two implementations: `Sink` (one tx
  per Write — for the daemon's HTTP path) and `BufferedSink` (chunked
  commits with row-by-row fallback — for batch importers). Both
  satisfy `events.Sink`; consumers depend on the interface.

- **Extractors are dispatched by an explicit registry.** Reading
  `events.DefaultExtractors()` tells you the full mapping of
  "Bash → ShellCommandExtractor, Read/Write/Edit/NotebookEdit →
  FilePathExtractor, WebFetch → WebFetchExtractor,
  Skill → SkillLoadExtractor". Each extractor body is field-mapping
  only; tool-name guards live in the registry.

- **`internal/agents` holds integration metadata, not event handling.**
  The `Agent` struct describes where on disk to install hooks and
  which hook events to subscribe to. Only `setup` / `teardown`
  CLIs consume it.

- **`api/openapi.yaml` is the source of truth for the wire**, not the
  Go types. The daemon enforces field constraints via
  `events.Envelope.Validate` in Go, but third-party agents reading
  the contract should target the YAML. The two are kept in lockstep.

### events vs ingest

`pkg/ingest` no longer exists; everything that used to live there
lives under `pkg/events` (with the same canonical type names). If
you find a stale reference in a doc or comment, treat it as drift
to fix. The migration was done in commits `7062490` (rename) and
`c1e4717` (flatten).

### Dependency direction

The arrow is "imports":

```
cmd/*                ──▶ internal/cli, internal/daemon
internal/cli         ──▶ pkg/events, pkg/events/sources/{claude,gemini},
                         internal/store, internal/daemon, internal/agents,
                         internal/mcp, pkg/llm, pkg/llm/prompts, pkg/redact,
                         internal/{config,paths,notify,nullable,preview,...}
internal/daemon      ──▶ pkg/events, internal/store
internal/agents      ──▶ (no internal deps)
internal/mcp         ──▶ pkg/events, internal/store, pkg/redact
internal/web         ──▶ pkg/events, internal/store, pkg/llm/prompts
internal/store       ──▶ pkg/events
pkg/llm/prompts      ──▶ pkg/events, pkg/llm, pkg/redact, internal/store
pkg/llm              ──▶ pkg/redact (egress scrub)
pkg/events/sources/* ──▶ pkg/events, internal/agents (slug only)
pkg/events           ──▶ pkg/redact (for ScannerRedactor adapter)
pkg/redact           ──▶ (no aichronicles deps)
```

Enforced rules:

| Rule | Status |
|---|---|
| `pkg/events` does not import `database/sql` | ✅ |
| `pkg/events` does not import `net/http` | ✅ |
| `pkg/events` does not import `internal/*` | ✅ |
| `internal/store` is the only SQL-aware package | ✅ |
| No import cycles | ✅ |

Mixed read-side discipline (currently): some types are
domain-clean (`EventView`, `Episode` in `pkg/events`); most
read-side types (`SessionDigestRow`, `LiveEvent`, `SubagentSpan`,
`SkillStaleness`, …) still live in `internal/store` with
`sql.NullString` fields. Consumers reach into `internal/store` for
those. Not broken, just inconsistent — see the "events vs store
read-side" comment in [data-flow.md](data-flow.md) for which type
lives where.

## The events Pipeline

`pkg/events.Pipeline` is the orchestrator. Three call sites use
it:

- **Daemon HTTP handler** (`internal/daemon/server.go`): one
  request, one event. Calls `Pipeline.Process(ctx, Event)` per
  request. Backed by the single-tx `store.Sink`.
- **Importers** (`internal/cli/import_{claude,gemini,jsonl}.go`):
  many events, one Pipeline.Run. Backed by `store.BufferedSink`
  with chunked commits.
- **Hook subprocess** (`internal/cli/ingest.go`): does NOT use
  Pipeline.Run — it uses just the Source's `HookTranslator` to
  produce one Envelope, then POSTs to the daemon. The Pipeline
  runs on the daemon side.

The Pipeline is stateless beyond its config fields:

```go
Pipeline{
    Sink:             store.NewSink(s),         // or NewBufferedSink
    Extractors:       events.DefaultExtractors(),
    RequireRedaction: true,                     // defense-in-depth
    Logger:           slog.Default(),
}
```

`Pipeline.Process` validates redaction, runs the extractor
registry, calls `Sink.Write`. `Pipeline.Run` consumes a Source
stream, calls Process per event with per-event error isolation,
calls `Sink.Flush` at the end, and reads `Sink.Stats()` for
final aggregates.

## Schema

Six tables (plus FTS5 + episodes), all in a single SQLite file at
`$XDG_STATE_HOME/aichronicles/store.db`. The schema diagram and
per-table rationale lives in
[`docs/reference/schema.md`](../reference/schema.md) (auto-
regenerated from the embedded migrations); not duplicated here so
the two can't drift.

Load-bearing invariants:

- **`raw_envelopes` is sacred.** Every accepted envelope is stored
  byte-for-byte in `envelope_json` *before* any projection.
  Replay-from-source is always possible; tests in
  `store/ingest_test.go` enforce stored bytes == POST body.
- **`sessions` is a derived rollup.** Created on first event for a
  session, then maintained by `AFTER INSERT ON events` triggers.
- **`events` is the queryable projection.** Typed columns extracted
  from the envelope JSON; FTS5 indexes `content_text`.
- **`extractions` is the typed-fact layer.** URLs, file paths,
  shell commands, skill loads. Computed by the `events.ExtractorRegistry`
  inside the SQLite Sink; written in the same transaction as the
  event. Discriminated by `kind` (`url`, `file_path`,
  `shell_command`, `skill_load`).
- **`episodes` is the segmenter's output.** Bounded session slices
  produced by `store.SegmentSession`, persisted via
  `SaveEpisodes`. Re-segmentable: DELETE-then-INSERT on each
  save, idempotent on the same input.
- **`llm_outputs` is the LLM-output cache.** Indexed by
  `(kind, prompt_hash)` — same prompt = cache hit, no API call.

## Migrations

Embedded SQL files under `internal/store/migrations/NNN_description.sql`,
versioned via the `meta` table's `schema_version` row. Each
migration runs in a single transaction; partial application is
impossible. Runner: `internal/store/migrate.go`.

Adding a new migration: drop a `NNN_*.sql` file, bump the meta
update at the bottom (`UPDATE meta SET value='N' WHERE
key='schema_version'`), update the `TestOpen_FreshCreatesSchema`
expectation. Then `make docs` regenerates
`docs/reference/schema.md`.

## LLM provider abstraction

Two adapters — `pkg/llm/anthropic.go` and `pkg/llm/openai.go` —
behind one provider-neutral interface
(`Client.Complete(ctx, Request) *Response`). Both wrap official
vendor SDKs:

| Concern | Anthropic adapter | OpenAI adapter |
|---|---|---|
| SDK | `github.com/anthropics/anthropic-sdk-go` | `github.com/openai/openai-go` |
| System prompt | TextBlockParam with cache_control: ephemeral | First message with `role: system` |
| Forced tool | `tool_choice={type:"tool",name:...}` | `tool_choice={type:"function",function:{name:...}}` |
| Strict schema | Inherent | `strict: true` on the function definition |
| Retries | SDK `option.WithMaxRetries` | SDK `option.WithMaxRetries` |
| Default model | `claude-sonnet-4-6` | `gpt-4o-mini` |

The adapters translate `llm.Request` ↔ SDK params on the way in,
and SDK response ↔ `llm.Response` on the way out. Tool input
schemas (raw `json.RawMessage` on our side) are mapped into
typed SDK structs. Tool-call output (typed object on Anthropic,
JSON string on OpenAI) is normalised to `json.RawMessage` so
callers unmarshal into the same `*Result` types either way.

The `prompt_hash` used for the `llm_outputs` cache is computed
on the provider-neutral `Request`, so switching providers does
not invalidate the cache for prompts that haven't actually
changed.

For switching providers in practice, see
[../how-to/switch-providers.md](../how-to/switch-providers.md).

## MCP server

`aichronicles mcp serve` is a stdio-attached JSON-RPC 2.0
server implementing a minimal subset of the
[Model Context Protocol](https://modelcontextprotocol.io). It
registers a set of read-only tools (`search_events`,
`list_sessions`, `find_episodes`, `get_summary`,
`list_subagents`, `get_unresolved_for_cwd`, `get_facts_for_subject`,
`search_with_summary`, `get_skill_staleness`, `get_project_context`,
`get_insights`, `list_skills`, `list_subagents`, `list_workflows`,
`find_fact_subjects`).

All tools read through `*store.Store` — no privileged writes. An
MCP client that compromises its sandbox still only reads stored
events, already scrubbed at ingest. Tool registration: `internal/mcp/tools_aichronicles.go`.

We deliberately do not depend on an external MCP SDK. The
protocol surface we use is small and stable, and self-contained
matches the rest of the tree's tight dependency posture.

## Trust boundaries (in code terms)

The threat model page describes trust boundaries narratively.
Where they're enforced in code:

- **Edge redaction (hook write path):**
  `pkg/events/sources/claude/hook.go::HookTranslator.Translate`
  and the equivalent in `gemini/hook.go`. Both apply their
  configured `events.Redactor` to the freshly-translated
  envelope, setting `Redaction.Applied=true` before returning.
- **Pipeline gate (any write path):**
  `pkg/events/pipeline.go::Pipeline.Process` rejects envelopes
  with `RequireRedaction=true` and missing/false `Redaction.Applied`.
  The daemon and importers both use `RequireRedaction=true`.
- **Daemon refusal (defense in depth):**
  `internal/daemon/server.go::handleIngest` translates
  `events.ErrRedactionRequired` into HTTP 400.
- **Store enforcement (last line of defense):**
  `internal/store/ingest.go::IngestEnvelopeWithExtractions`
  returns `ErrRedactionRequired` even for callers that bypass
  the Pipeline.
- **LLM-output write:**
  `internal/store/llm_outputs.go::SaveLLMOutput` scrubs the
  body through `redact.Outbound` before insertion. The edge
  redactor never sees an LLM response, so the store is the
  enforcement point for LLM-egress data.
- **LLM-egress redaction (outbound network):** prompt builders
  in `pkg/llm/prompts/prompts.go` route every user-content
  string through `redact.Outbound` before composing the user
  message.
- **API error scrub:** `pkg/llm/anthropic.go::scrubAnthropicError`
  and `pkg/llm/openai.go::scrubOpenAIError`.
- **Config-file mode check:** `internal/config/config.go::LoadFrom`
  refuses 0644-or-permissive files when any provider has
  `api_key_command` set.

Note: line numbers were intentionally removed from this list —
they drift constantly. Use `grep` against the function names
above; they're stable.

## Related

- [Data flow](data-flow.md) — sequence diagrams for ingest, summarize, induction, meta sweep.
- [Threat model](threat-model.md) — what this defends and what it doesn't.
- [Schema reference](../reference/schema.md) — auto-generated table definitions.
