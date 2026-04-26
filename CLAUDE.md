# aichronicles — development practices

These rules are load-bearing. Follow them for every change.

## 1. Conventional commit messages

Use `type(scope): summary` — e.g. `feat(ingest): add /v1/ingest handler`, `fix(store): handle nil session_id`, `docs(openapi): document redaction behavior`.

Allowed types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `perf`, `build`, `ci`, `revert`. Subject line ≤ 72 chars, imperative mood, no trailing period.

## 2. Small commits, one logical change

Each commit does exactly one thing and leaves the tree green (`go build`, `go test`, lints all pass). Split mechanical changes (renames, moves) from behavior changes. If a commit message needs "and", it should probably be two commits.

## 3. Idiomatic Go, current language features

Target the toolchain in `go.mod`. Prefer stdlib first. Specifically:

- `log/slog` for structured logging (no third-party loggers)
- `errors.Is` / `errors.As` / `%w` wrapping (no string matching)
- `context.Context` as the first argument of anything that can block, I/O, or be cancelled
- `net/http` with Go 1.22+ `ServeMux` patterns (no web framework by default)
- Generics only where they remove real duplication, never for fashion
- `any` over `interface{}`
- No `init()` for business logic

Run `gofmt`, `go vet`, `staticcheck` (or `golangci-lint`) before every commit.

## 4. Unit tests for every feature

Every new function or behavior ships with tests in the same package (`_test.go`). Prefer table-driven tests. Use `t.Run` for subtests, `t.Parallel()` where safe, `testing.T.Helper()` in helpers. No external test deps unless there's a clear reason.

A PR without tests for new logic is not ready.

## 5. Integration tests

Cover the seams that unit tests can't: real SQLite, the UDS listener, the ingest CLI end-to-end. Live under `./integration/...` with a build tag (`//go:build integration`) so `go test ./...` stays fast and `go test -tags=integration ./...` runs the full suite. CI runs both.

## 6. Never guess — always root-cause

When a test fails, a build breaks, or a behavior is wrong: read the code, read the error, reproduce it, and understand *why* before changing anything. No "try this and see." No plausible-sounding fixes. No broad catch-and-ignore. If you can't explain the failure in one sentence, you haven't found the cause yet.

## 7. Correctness beats coverage

When a feature can be built two ways — narrow but always right, or broad but sometimes wrong — choose narrow. Wrong stored data (a synthesised URL pointing at the wrong PR, a "key file" the session never touched, a link the LLM hallucinated) is worse than missing data: it produces confidently-wrong results that are hard to spot and harder to debug.

Concretely:

- Only synthesise a value when its inputs unambiguously determine it. If resolving a reference needs context that might be stale, missing, or guessed-at, capture the bare fact and defer resolution.
- Anti-fabrication grounding (the "Links observed" / "Files observed" prompt pattern) beats soft "please don't invent" instructions. When the model is asked to enrich captured data, give it the canonical list and tell it to drop entries it can't attribute.
- Heuristic shortcuts like "the cwd's git remote at extract time tells us the repo" or "GitHub's redirect papers over `/pull/` vs `/issues/`" almost always fail silently in some corner. Skip the case rather than ship the heuristic.
- "Returning nothing" is a valid answer. If extraction can't determine a value cleanly, drop the row; the user (or a later layer like an LLM) can do the inference better than a regex.
