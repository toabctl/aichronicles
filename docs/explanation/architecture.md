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
shape with three rings:

  - **Domain core** (`internal/events`) at the center — no SQL,
    no HTTP, no I/O.
  - **Wire layer** (`internal/wire`) one ring out — typed
    request/response shapes for the HTTP API, transport-agnostic.
  - **Infrastructure adapters** (`internal/store` for SQLite,
    `internal/api` for the HTTP daemon, `internal/apiclient`
    for Go clients of the daemon, `internal/web`, `internal/mcp`).
  - **Application orchestration** on top (`internal/cli`,
    `cmd/*`).

```
                ┌─────────────────────────────────────────────┐
 Entry points   │ cmd/aichronicles    cmd/aichronicles-api    │
                └────────┬─────────────────────────┬──────────┘
                         │                         │
                ┌────────▼──────┐         ┌────────▼─────────┐
 Application    │ internal/cli  │  HTTP   │  internal/api    │
 (orchestration)│ (cobra cmds)  │◀───────▶│  (HTTP + UDS,    │
                │               │  /v1/*  │   SSE bus, web)  │
                └──┬────────────┘         └──────┬───────────┘
                   │                             │
              ┌────▼───────────┐         ┌───────▼────────┐
 Client       │internal/       │         │ internal/store │  ← SQLite-bound
 adapter      │ apiclient      │         │   events.Sink  │     events.Sink
              │ (typed, UDS)   │         │   lives here   │
              └────────────────┘         └───────┬────────┘
                                                 │
                                                 ▼
              ┌──────────────────┐    ┌──────────────────┐
 Wire layer   │   internal/wire  │    │ internal/events  │  ← Domain core
              │ Problem, Cursor, │    │ envelope, kind,  │     no SQL,
              │ EventList,       │    │   episode, view, │     no HTTP, no I/O
              │ SessionDigest,   │    │   redact, role,  │
              │ SearchHit,       │    │   nullable       │
              │ ... (per-feature)│    │                  │
              │ JSON tags only,  │    │   source, sink,  │  ← interfaces
              │ no sql, no http  │    │   extractor,     │
              └──────────────────┘    │   pipeline       │
                                      │                  │
                                      │   sources/       │  ← concrete sources
                                      │     claude/      │
                                      │     gemini/      │
                                      │     codex/       │
                                      └──────────────────┘
```

## Process model

One daemon, plus systemd timers for periodic work. (Pre-2026-05
the system shipped two daemons — `aichroniclesd` for ingest only,
plus the read-side opening SQLite directly. The api
rearchitecture collapsed that into a single store-owning process.)

| Binary | Process kind | Lifecycle | Owns |
|---|---|---|---|
| `aichronicles-api` | Long-running daemon | systemd `--user` unit, socket-activated | The single SQLite-handling process. Serves `/v1/*` JSON read+write API and `/v1/stream` SSE live activity. Server-side redaction; the only point of "no unredacted bytes in storage" enforcement. |
| `aichronicles-web` | Long-running daemon | systemd `--user` unit (`aichronicles-web.service`) | Renders `/` web HTML. Reads via `internal/apiclient` against `aichronicles-api`'s UDS — separate process so a web-handler bug can't take down ingest. |
| `aichronicles hook` | Short-lived hook subprocess | Forked per Claude Code / Gemini CLI / Codex CLI hook event | One envelope's worth of translation + POST to `aichronicles-api`. Translation is pure (post-Phase 0); the api applies redaction. Fire-and-forget: every error path logs and exits 0 so the hook never breaks the agent's prompt loop. |
| `aichronicles induction sweep` | Short-lived periodic | `aichronicles-cron-induction.timer` (`OnCalendar=*-*-* 09,21:00:00`, twice daily) | Single-session induction across idle sessions |
| `aichronicles meta sweep` | Short-lived periodic | `aichronicles-cron-meta-analysis.timer` (1h poll, per-kind cadences in SQLite) | Cadence-gated meta-analyses (propose / reflect / challenge / digest_weekly / skill_revision) |
| `aichronicles digest weekly` | Short-lived periodic | `aichronicles-cron-weekly-digest.timer` (`OnCalendar=Mon 06:00:00 UTC`) | Weekly retrospective digest |
| `aichronicles prune --yes` | Short-lived periodic | `aichronicles-cron-prune.timer` (`OnCalendar=Sun 04:00:00 UTC`) | Retention: drops sessions past `--older-than` (five years by default — a backstop against pathological growth, not routine hygiene; the corpus is what the LLM pipelines reason over) and their cascade. Sunday so it never contends with the Monday digest for the write lock. |
| `aichronicles <other>` | Short-lived user CLI | Forked per command | Read paths, imports, summarize, MCP server (`mcp serve`), web (`web serve`) |

Why this shape:

- **The api is the only SQLite-handling process.** Reads, writes,
  ingest, and SSE all share one open handle. No cross-process
  notification needed for live activity (the in-process SSE bus
  fans out from the same goroutine that committed the write).
- **The hook subprocess is fire-and-forget by design.** A wedged
  api can never block a hook; the CLI exits 0 even on POST
  failures and flips a transport-error outage flag (HTTP 4xx/5xx
  do NOT flip it — those mean the api is up but the envelope
  was rejected, and treating that as "daemon unreachable" would
  produce false positives on validation drift).
- **Server-side redaction is the only redaction.** Sources are
  pure translators; the api's `events.Pipeline` runs the
  Redactor inline before the Sink, and stores the
  re-marshaled post-redaction bytes in `raw_envelopes`. A
  client that lies about `redaction.applied=true` cannot
  smuggle secrets past the gate.
- **Periodic work runs as systemd-timer CLI subcommands.**
  Failure isolation, per-run journal logs, and suspend
  catch-up via `Persistent=true` are properties the
  in-process ticker couldn't give.

### Write-ownership contract

The api daemon owns the INGEST PATH end-to-end: every write to
`raw_envelopes`, `events`, `ingest_pending`, `extractions`,
`sessions`, and `session_outcomes` goes through `handleIngest` →
`IngestWorker` → `events.Pipeline.Process` inside the daemon
process. The hook subprocess and every import path are clients of
`/v1/ingest`.

Two classes of CLI commands write directly to the SQLite file
outside the daemon. Each class has a defined contract:

**Class 1 — maintenance commands that rewrite daemon-owned
tables** refuse to run while the daemon is up. They call
`cli.RefuseIfDaemonRunning` before opening the store; if
`/v1/healthz` answers, they error with a clear stop-instruction.
Today that's `aichronicles backfill-extractions` (rewrites the
extractions table). `aichronicles scrub` is in the same conceptual
class but routes through `POST /v1/scrub` instead of opening the
store, so it inherits the daemon's write lock.

There is no Class 2. This document previously described the LLM
commands (`propose`, `propose add/merge/discard`, `induction`,
`summaries`, `meta sweep`) as opening the store directly to INSERT
into `llm_outputs` / `skill_candidates` / `semantic_facts`, and
justified it on bandwidth grounds. That was already false when
written: `runCachedLLM` persists via `POST /v1/llm-outputs`, and
depcheck's call-rule would fail CI on a direct `store.Save*` from
`internal/cli`.

The honest statement is: **the api daemon owns every write.**
`backfill-extractions` is the single exception, and it refuses to
run while the daemon is up.

### Read-side: through apiclient

Every non-test file under `internal/cli/`, `internal/mcp/`, and
`internal/web/` reads through `internal/apiclient`. "Every read
goes through the api" is a hard invariant, enforced by
`tools/depcheck`:

- An import-graph rule blocks `internal/apiclient` from importing
  `internal/store` (apiclient is wire-only).
- Code-pattern rules scan non-test `.go` files under each of the
  three packages for `store.(Load|Save|Insert|Update|Delete|Has|
  Last|Query|Vacuum|Segment)\w*\(` calls and fail CI on a hit.
  Test files are exempt because they exercise the store directly
  for fixture setup and state assertions; doc-comment lines are
  also skipped.

Most CLI subcommands no longer accept a `--db` flag; they take
`--socket` to point at the aichronicles-api UDS instead. The
exceptions are `backfill-extractions` and `scrub`, the two
maintenance commands that genuinely open the file — both refuse to
run while the daemon is up.

A depcheck call-rule forbids `.DB()` anywhere under `internal/cli`
outside those two files, so the invariant is checked rather than
asserted. `internal/cli` still imports
`internal/store` for type/enum constants (`LLMOutputKind`,
`SkillKind`, `OutcomeLabel`, error sentinels) — those are
package-level values, not CRUD calls, and the depcheck regex
ignores them.

## Package map

```
cmd/
  aichronicles/         CLI multiplexer entrypoint
  aichronicles-api/     api daemon entrypoint (UDS HTTP server:
                        /v1/* JSON, /v1/stream SSE — write-owner)

internal/               private; only this binary imports
  wire/                 wire types — request/response shapes for the
                        HTTP API, transport-agnostic. JSON tags on
                        every exported field; *string / *int64 for
                        nullable columns (no sql.NullString); RFC
                        7807 Problem error shape; Cursor/PageRequest/
                        PageResponse pagination contract; per-feature
                        types live in events.go, sessions.go,
                        episodes.go, search.go, facts.go, insights.go,
                        skills.go, llm_outputs.go, projects.go,
                        unresolved.go, subagents.go, writes.go,
                        stream.go. CI guard: internal/wire does not
                        import database/sql or net/http.
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
                        (shared by Claude, Gemini and Codex hooks)
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
      codex/
        hook.go         HookTranslator (hooks only — Codex's
                        rollout files have no importer yet)
  redact/               detector library + scanner combinators
                        (no other internal/* deps)
  llm/                  provider-neutral interface
    config.go           Provider / Config / FromConfig switchboard
    anthropic.go        adapter using anthropic-sdk-go
    openai.go           adapter using openai-go
    prompts/            BuildSummary / BuildReflect / BuildPropose
  agents/               Claude Code / Gemini CLI / Codex CLI metadata
                        (slug, hook event names, per-event timeout
                        overrides, settings.json paths — consumed
                        only by `setup` / `teardown`)
  api/                  HTTP daemon: server, mux, handlers per feature,
                        SSE bus, redaction-server-side ingest path.
                        Replaces and absorbs the legacy internal/daemon.
                        Files:
                          server.go         Server, NewServer, mux,
                                            timeouts, ListenAndServe,
                                            Serve, gracefulShutdown,
                                            handleIngest+handleHealthz,
                                            writeProblem/writeJSON
                          systemd.go        ListenFromSystemd
                          watchdog.go       WATCHDOG_USEC handling
                          sse_bus.go        in-process pub/sub
                          handler_events.go         GET /v1/events
                          handler_sessions.go       GET /v1/sessions{,/{id},/{id}/related}
                          handler_episodes.go       GET /v1/episodes
                          handler_search.go         GET /v1/search
                          handler_facts.go          GET /v1/facts{,/subjects}
                          handler_skills.go         GET /v1/skills/staleness
                          handler_misc.go           GET /v1/{summaries,llm-outputs,unresolved,
                                                     subagents,insights,projects/aggregates}
                          handler_writes.go         POST /v1/{llm-outputs,episodes,facts,
                                                      session-outcomes,session-links}
                          handler_stream.go         GET /v1/stream (SSE)

  apiclient/            typed Go client for aichronicles-api.
                        UDS-dialing http.Client, RFC 7807 problem
                        decoding, sentinel errors (ErrSocketUnavailable
                        / ErrNotFound / ErrTooLarge / ErrConflict /
                        ErrServer) + structural HTTPError. One file
                        per feature: client.go, errors.go, ingest.go,
                        events.go, sessions.go, episodes.go, search.go,
                        facts.go, skills.go, misc.go, writes.go.
                        CI guard (Phase D): apiclient does not import
                        internal/store.

  cli/                  cobra subcommands. Reads go through
                        internal/apiclient; a handful of LLM-cache and
                        maintenance commands still open the store
                        directly (see "Read-migration" above).
    apiclient.go        openAPIClient(sockFlag) shared helper
    hook.go             hook subprocess body (was ingest.go);
                        translators are pure, daemon redacts
    setup*.go,          systemd + agent-hook wiring (defaultHookCommand
    teardown*.go        is "aichronicles hook"; teardown also removes
                        legacy aichronicles.{service,socket})
    import_claude.go,   bulk import paths — each builds an
    import_gemini.go,   events.Pipeline + events.Source +
    import_jsonl.go     store.BufferedSink and calls Pipeline.Run.
    summarize.go,       LLM-using subcommands. Reads through
    reflect.go,         apiclient; write llm_outputs / skill_candidates
    propose*.go,        directly via openStore (see #1 in the open
    induction.go,       arch debt).
    facts.go,
    digest.go,
    insights.go,
    skills_*.go
    audit.go, scrub.go  redaction inspection / in-place rewrite
    search.go           FTS5 search
    sessions.go         session listing
    unresolved.go       talks to /v1/unresolved via apiclient
    mcp_serve.go        MCP host (cobra wrapper around internal/mcp)
    meta_sweep.go       cadence-gated meta-analyses dispatcher
    assets/             embedded systemd unit files
                        (aichronicles-api.{service,socket} for the
                        api daemon, aichronicles-web.{...} for the
                        HTML browser, aichronicles-cron-*.{service,timer}
                        for periodic work)

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

- **`internal/events` is the domain core.** It imports nothing from
  `internal/*`, no `database/sql`, no `net/http`. It owns the event
  model end-to-end: shape, validation, pipeline, sources, sinks,
  extractors, and the read-side primitives (`EventView`, `Episode`).
  The package has no stability promise but is intentionally
  embeddable.

- **Sources are sub-packaged by agent.** `internal/events/sources/claude`,
  `internal/events/sources/gemini` and `internal/events/sources/codex`
  each export a `HookTranslator` (single-shot, used by `aichronicles
  hook`); claude and gemini also export a `JSONLSource` /
  `TranscriptSource` (streaming, used by importers). Codex ships the
  hook half only. Adding an agent = one new sub-package; nothing in
  `internal/events` changes — the Codex integration bore this out,
  needing only one new `case` in `translateHook`.

- **The Sink interface lives in `internal/events`; the SQLite implementation
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

- **No LLM calls run on the web request path.** `internal/web`
  imports `internal/llm/prompts` only for the JSON Result types
  (`SummaryResult`, `ProposalResult`, `ReflectionResult`,
  `InductionResult`) that parse cached llm_outputs bodies. The
  `internal/llm` package itself — the Anthropic/OpenAI client
  switchboard — is not imported anywhere under `internal/web`.
  Every LLM call lives in a CLI subprocess (`reflect`, `propose`,
  `induction`, `summaries`, `meta sweep`); the web only renders
  the cached results those subcommands persist. This keeps the
  web's request budget bounded to template render + apiclient
  round-trip and side-steps the "slow API call holds the
  connection" failure mode entirely.

### events vs ingest

`pkg/ingest` no longer exists; everything that used to live there
lives under `internal/events` (with the same canonical type names). If
you find a stale reference in a doc or comment, treat it as drift
to fix. The migration was done in commits `7062490` (rename) and
`c1e4717` (flatten). A subsequent move flattened pkg/{api,events,llm,redact}
into internal/* (with pkg/api → internal/wire to clear the
internal/api naming clash).

### Dependency direction

The arrow is "imports":

```
cmd/aichronicles-api ──▶ internal/api
cmd/aichronicles     ──▶ internal/cli
internal/cli         ──▶ internal/events, internal/events/sources/{claude,gemini,codex},
                         internal/store, internal/apiclient, internal/wire,
                         internal/agents, internal/mcp, internal/llm,
                         internal/llm/prompts, internal/redact,
                         internal/{config,paths,notify,nullable,preview,...}
internal/api         ──▶ internal/wire, internal/events, internal/store, internal/redact
internal/apiclient   ──▶ internal/wire, internal/events
internal/agents      ──▶ (no internal deps)
internal/mcp         ──▶ internal/apiclient, internal/wire, internal/llm,
                         internal/llm/prompts, internal/skills, internal/preview,
                         internal/timefmt
                         (cross-process; reads via the api UDS)
internal/web         ──▶ internal/apiclient, internal/wire, internal/preview,
                         internal/timefmt
                         (cross-process daemon — aichronicles-web.service —
                          fronting the api UDS; the previous "reads SQLite
                          directly via WAL" arrow was the pre-split design)
internal/store       ──▶ internal/events, internal/wire, internal/nullable, internal/redact
internal/llm/prompts      ──▶ internal/events, internal/llm, internal/redact, internal/wire
internal/llm              ──▶ internal/redact (egress scrub)
internal/events/sources/* ──▶ internal/events, internal/agents (slug only)
internal/wire              ──▶ (stdlib only)
internal/events           ──▶ internal/redact (for ScannerRedactor adapter)
internal/redact           ──▶ (no aichronicles deps; enforced by depcheck)
```

Enforced rules:

| Rule | Status |
|---|---|
| `internal/events` does not import `database/sql` | ✅ |
| `internal/events` does not import `net/http` | ✅ |
| `internal/events` does not import `internal/*` | ✅ |
| `internal/wire` does not import `database/sql` | ✅ |
| `internal/wire` does not import `net/http` | ✅ |
| `internal/wire` does not import `internal/*` | ✅ |
| `internal/store` is the only SQL-aware package | ✅ |
| `internal/apiclient` does not import `internal/store` | ✅ |
| No import cycles | ✅ |

All of the above are checked in CI by `tools/depcheck`, not merely
asserted here — with one caveat: "`internal/store` is the only
SQL-aware package" is currently true of every package except
`internal/skills`, which mixes filesystem discovery with raw SQL
against `events` / `extractions`. depcheck cannot see it because
the call-rules match on the `store.` qualifier. Moving those three
functions into `internal/store` would leave `internal/skills` a
pure filesystem leaf and let the rule be made absolute.

Mixed read-side discipline (currently): some types are
domain-clean (`EventView`, `Episode` in `internal/events`); most
read-side types (`SessionDigestRow`, `LiveEvent`, `SubagentSpan`,
`SkillStaleness`, …) still live in `internal/store` with
`sql.NullString` fields. Consumers reach into `internal/store` for
those. Not broken, just inconsistent — see the "events vs store
read-side" comment in [data-flow.md](data-flow.md) for which type
lives where.

## The events Pipeline

`internal/events.Pipeline` is the orchestrator. Three call sites use
it:

- **API HTTP handler** (`internal/api/server.go`): one
  request, one event. Calls `Pipeline.Process(ctx, Event)` per
  request. Backed by the single-tx `store.Sink`.
- **Importers** (`internal/cli/import_{claude,gemini,jsonl}.go`):
  many events, one Pipeline.Run. Backed by `store.BufferedSink`
  with chunked commits.
- **Hook subprocess** (`internal/cli/hook.go`): does NOT use
  Pipeline.Run — it uses just the Source's `HookTranslator`
  to produce one Envelope, then POSTs to aichronicles-api via
  `apiclient.Client.Ingest`. The Pipeline runs on the api side.

The Pipeline is stateless beyond its config fields:

```go
Pipeline{
    Sink:       store.NewSink(s),                            // or NewBufferedSink
    Extractors: events.DefaultExtractors(),
    Redactor:   events.NewScannerRedactor(redact.Default()), // required
    Logger:     slog.Default(),
}
```

`Pipeline.Process` runs the Redactor in place (re-marshaling
`e.Raw` so post-redaction bytes are what the Sink stores),
runs the extractor registry, and calls `Sink.Write`. A nil
Redactor returns `events.ErrRedactionRequired`. `Pipeline.Run`
consumes a Source stream, calls Process per event with
per-event error isolation, calls `Sink.Flush` at the end, and
reads `Sink.Stats()` for final aggregates.

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

Two adapters — `internal/llm/anthropic.go` and `internal/llm/openai.go` —
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
| Default model | `claude-opus-4-7` | `gpt-4o-mini` |

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

All tools read through `internal/apiclient` against
`aichronicles-api` over UDS — no privileged writes, no direct
SQLite access from the MCP process. An MCP client that compromises
its sandbox still only reads stored events, already scrubbed at
ingest. Tool registration entry points: `internal/mcp/tools_apiclient.go`
(`RegisterAichroniclesAPITools`, the bulk of the catalog),
`internal/mcp/tools_analytics.go`, `internal/mcp/tools_aichronicles_llm.go`;
helpers in `internal/mcp/tools_aichronicles.go`.

We deliberately do not depend on an external MCP SDK. The
protocol surface we use is small and stable, and self-contained
matches the rest of the tree's tight dependency posture.

## Trust boundaries (in code terms)

The threat model page describes trust boundaries narratively.
Where they're enforced in code:

- **Server-side redaction (THE enforcement point):**
  `internal/events/pipeline.go::Pipeline.Process` runs the
  configured `events.Redactor` on every envelope before
  extractor dispatch and Sink.Write, then re-marshals
  `e.Raw` from the post-redaction Envelope so the SQLite
  Sink stores scrubbed bytes. A `nil` Redactor returns
  `events.ErrRedactionRequired` — fail loud rather than
  silently store unredacted bytes. The api daemon's
  `internal/api/server.go::NewServer` configures the
  Pipeline with `redact.Default()` unconditionally.
- **No client trust:** the hook subprocess and the
  importers ship raw envelopes; the api applies redaction
  regardless of any `redaction.applied` claim on the wire.
  A buggy or malicious client cannot smuggle secrets past
  the gate by lying about pre-redaction.
- **Sources are pure translators:** `internal/events/sources/{claude,
  gemini,codex}/{hook,jsonl,transcript}.go` hold no Redactor
  field. Translation produces an unredacted envelope; the
  consuming Pipeline scrubs. Each source carries a test asserting
  its translator emits `Redaction == nil`, so a source that
  re-acquires one fails rather than quietly double-scrubbing.
- **Store enforcement (last line of defense):**
  `internal/store/ingest.go::IngestEnvelopeWithExtractions`
  returns `ErrRedactionRequired` even for callers that
  somehow reach the Sink without going through the Pipeline.
- **LLM-output write:**
  `internal/store/llm_outputs.go::SaveLLMOutput` scrubs the
  body through `redact.Outbound` before insertion. The edge
  redactor never sees an LLM response, so the store is the
  enforcement point for LLM-egress data.
- **LLM-egress redaction (outbound network):** prompt builders
  in `internal/llm/prompts/prompts.go` route every user-content
  string through `redact.Outbound` before composing the user
  message.
- **API error scrub:** `internal/llm/anthropic.go::scrubAnthropicError`
  and `internal/llm/openai.go::scrubOpenAIError`.
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
