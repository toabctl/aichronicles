package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/nullable"
)

// SearchOrder selects how SearchEvents orders its result rows.
type SearchOrder int

const (
	// OrderRank sorts by a recency-boosted FTS5 relevance score.
	// Concretely: bm25(...) / pow(2, days_old / recencyHalfLifeDays).
	// bm25 is lower-is-better (typically negative); dividing by a
	// value > 1 pushes older rows toward zero (worse) without
	// flipping the sign, so a strongly relevant old event still
	// beats a weakly relevant new one but two equally relevant
	// events resolve in favour of the more recent. Exponential
	// (rather than linear) decay so the constant honestly maps to
	// "the score halves every recencyHalfLifeDays days." Default
	// for the CLI.
	OrderRank SearchOrder = iota

	// OrderRecency sorts by ts_source_ms DESC (newest first),
	// ignoring relevance entirely. Default for the MCP
	// search_events tool, where an agent asking "did I work on X
	// recently?" wants chronological order.
	OrderRecency
)

// recencyHalfLifeDays is the half-life of the recency-boost decay:
// the score halves for every recencyHalfLifeDays of age. Day 0 →
// divisor 1; day H → 2; day 2H → 4; day 3H → 8. Larger H flattens
// the curve (older rows penalised less); smaller H steepens it.
// 30 days picked as a default — week-old work still ranks well,
// month-plus-old work drops by half, three-months-old drops to an
// eighth. Exposed as a const so it can be tuned without grepping
// for magic numbers.
//
// The previous formula was linear (1 + days_old/30) and was
// (mis)named recencyHalfDays despite producing harmonic decay
// (day 30 → ½, day 60 → ⅓, day 90 → ¼). Exponential decay is
// honest about what the constant means and matches the standard
// retrieval-system convention (e.g. Elasticsearch's gauss/exp
// score function defaults).
const recencyHalfLifeDays = 30.0

// recencyBoostedRankExpr returns the SQL expression that takes an
// FTS5 bm25 rank column and a ts_source_ms column and produces a
// recency-boosted score:
//
//	rank / MAX(1.0, pow(2.0, days_old / recencyHalfLifeDays))
//
// bm25 is lower-is-better (typically negative), so dividing by a
// value > 1 pushes older rows toward zero without flipping sign.
// Uses SQLite's pow(2, x) — supplied by the modernc.org/sqlite
// math extension — for a true 2^x curve.
//
// The MAX(1.0, ...) clamp guards against future-dated events: a
// clock-skewed ts_source_ms greater than the now bind would push
// the divisor below 1 and could invert the ranking, ranking bogus
// rows infinitely high. Caller must bind the now_ms value before
// LIMIT in the args slice.
//
// Centralised so the three sites that build search SQL (NoDedup,
// dedup, searchExtractions) can't drift on the formula.
func recencyBoostedRankExpr(rankCol, tsCol string) string {
	return fmt.Sprintf(
		`%s / MAX(1.0, pow(2.0, ((? - %s) / 86400000.0) / %.1f))`,
		rankCol, tsCol, recencyHalfLifeDays,
	)
}

// SearchEventOpts is the input contract for SearchEvents.
//
// Query must already be a syntactically-valid FTS5 MATCH expression;
// pass it through internal/searchquery.ToFTS5 first if it came from
// human or agent input.
type SearchEventOpts struct {
	Query      string
	Kind       string
	SessionID  string
	SubagentID string
	SinceMs    int64
	Limit      int
	NoDedup    bool
	Order      SearchOrder

	// Offset skips this many result rows before the Limit window —
	// the OFFSET half of pagination. Applied on the OUTER query
	// (after dedup), with a unique rowid tiebreaker on every ORDER BY
	// so paging never skips or duplicates a row. Zero for the first
	// page.
	Offset int

	// Stage locks which FTS fallback stage to query (StagePrimary /
	// StageTrigram / StageExtractions). Empty selects the stage the
	// normal way — try primary, then trigram, then extractions, first
	// non-empty wins — and SearchEventsPaged reports which one it
	// picked so the cursor can pin it. A non-empty Stage queries ONLY
	// that stage with no fallback, so a deep page that runs off the
	// end returns nothing instead of leaking the next corpus's rows.
	Stage string

	// SourceAgent narrows to events whose source_agent matches
	// (exact). Useful for "what did I ask Claude vs Gemini about
	// this week" queries.
	SourceAgent string

	// ToolName narrows to events with a matching tool_name (exact).
	// E.g. "Bash" or "run_shell_command".
	ToolName string

	// SkillName narrows to events whose session loaded the named
	// skill (joins extractions kind=skill_load value=?). Session-
	// level: returns every matching event from sessions where the
	// skill fired, not just the skill_load event itself.
	SkillName string

	// FilePathSubstring narrows to events whose session touched a
	// file matching this substring (joins extractions
	// kind=file_path value LIKE %?%). Session-level by design —
	// surfacing every event in a session that worked on the file
	// is more useful than only the Read/Write events themselves.
	FilePathSubstring string

	// WithFailures, when true, narrows to events whose session
	// produced at least one tool_failure event. Pairs with the
	// staleness detector — patterns where the agent's tool calls
	// fail.
	WithFailures bool

	// NowMs anchors the recency-boost calculation for OrderRank.
	// Zero means use time.Now().UnixMilli() at call time. Set
	// explicitly in tests so result ordering is deterministic.
	NowMs int64
}

// SearchEventHit is one row returned by SearchEvents. Cwd, Content,
// and Snippet are nullable to mirror the underlying schema and FTS5
// helper output; callers decide how to render NULLs for their format.
//
// Snippet is computed by SQLite's snippet() function and centers on
// the matched terms, capped at a fixed token count. Prefer it over
// Content for hit display: it's tokenizer-aware and shows what the
// user was looking for rather than the start of the document.
type SearchEventHit struct {
	SessionID string
	Kind      string
	// Cwd / Content / Snippet are optional — see
	// arch_review_2026_05_13 MEDIUM #10. Cwd is nil for sessions
	// that never had one captured; Content for purely metadata
	// kinds; Snippet for results from queries that didn't
	// produce a SQLite snippet() call.
	Cwd        *string
	TsSourceMs int64
	Content    *string
	Snippet    *string
}

// FTS index names. Three virtual tables back search:
//   - events_fts uses unicode61 with code-friendly separators; this
//     is the primary path for whole-word and identifier-aware queries.
//   - events_fts_trigram uses the trigram tokenizer for substring
//     matches, consulted when the primary returns nothing.
//   - extractions_fts indexes the typed-facts table (URLs, file
//     paths, shell commands), consulted last so a search like
//     `migrate.go` finds sessions that touched the file via Read
//     even when no message text mentions it.
const (
	indexPrimary     = "events_fts"
	indexTrigram     = "events_fts_trigram"
	indexExtractions = "extractions_fts"
)

// Stage identifiers for the FTS fallback stage a search resolved to.
// Carried in the pagination cursor (SearchEventOpts.Stage) so every
// page of one pagination queries the same corpus. Values are stable
// wire-visible strings — do not renumber/rename without a cursor
// version bump.
const (
	StagePrimary     = "primary"
	StageTrigram     = "trigram"
	StageExtractions = "extractions"
)

// SearchEvents runs an FTS5 MATCH against events_fts and returns the
// matching rows.
//
// Default behaviour wraps the FTS hits in a CTE + ROW_NUMBER window
// so logical turns captured through multiple sources (a hook event
// plus its later transcript import) collapse to one row per
// (session_id, role, kind, content_text). Within a partition,
// transport='hook' wins, then FTS rank breaks ties. Set NoDedup to
// see every underlying row — useful for auditing what's actually in
// the store.
//
// If the primary index returns zero hits, SearchEvents transparently
// retries against the trigram index. The fallback handles the
// "I typed `MongoD` but the corpus has `MongoDB`" case without
// paying the trigram cost on every query.
//
// Order picks between FTS rank (most relevant first) and recency
// (newest first); future commits will replace both with a blended
// score, but the parameter stays so callers can opt in or out.
func SearchEvents(ctx context.Context, db *sql.DB, opts SearchEventOpts) ([]SearchEventHit, error) {
	hits, _, err := SearchEventsPaged(ctx, db, opts)
	return hits, err
}

// SearchEventsPaged is SearchEvents with pagination support. It
// returns the matching rows AND the FTS stage they came from, so a
// caller building a pagination cursor can pin the stage for every
// subsequent page.
//
// Stage selection:
//   - opts.Stage == "": first-page behaviour. Probe each stage in
//     order (primary → trigram → extractions) with a LIMIT-1 / OFFSET-0
//     existence check, lock the first stage that has any match, then
//     run the real Limit/Offset query against ONLY that stage. The
//     probe ignores opts.Offset on purpose: a deep offset must not
//     make a non-empty stage look empty and fall through to the next
//     corpus.
//   - opts.Stage != "": query ONLY that stage with no fallback. An
//     empty page just means the locked stage is exhausted (end of
//     results), never a fall-through to a different corpus.
//
// When nothing matches in any stage, the returned stage is
// StagePrimary (arbitrary — the empty result yields no next page).
func SearchEventsPaged(ctx context.Context, db *sql.DB, opts SearchEventOpts) ([]SearchEventHit, string, error) {
	if strings.TrimSpace(opts.Query) == "" {
		return nil, "", fmt.Errorf("SearchEvents: query is required")
	}
	if opts.NowMs == 0 {
		opts.NowMs = time.Now().UnixMilli()
	}

	if opts.Stage != "" {
		hits, err := runSearchStage(ctx, db, opts, opts.Stage)
		return hits, opts.Stage, err
	}

	for _, stage := range []string{StagePrimary, StageTrigram, StageExtractions} {
		probe := opts
		probe.Limit = 1
		probe.Offset = 0
		found, err := runSearchStage(ctx, db, probe, stage)
		if err != nil {
			return nil, "", err
		}
		if len(found) == 0 {
			continue
		}
		hits, err := runSearchStage(ctx, db, opts, stage)
		if err != nil {
			return nil, "", err
		}
		return hits, stage, nil
	}
	return nil, StagePrimary, nil
}

// runSearchStage executes one named FTS stage. Centralises the
// stage→builder mapping so SearchEventsPaged's probe and real-query
// paths can't drift.
func runSearchStage(ctx context.Context, db *sql.DB, opts SearchEventOpts, stage string) ([]SearchEventHit, error) {
	switch stage {
	case StagePrimary:
		return searchAgainst(ctx, db, opts, indexPrimary)
	case StageTrigram:
		return searchAgainst(ctx, db, opts, indexTrigram)
	case StageExtractions:
		return searchExtractions(ctx, db, opts)
	default:
		return nil, fmt.Errorf("SearchEvents: unknown stage %q", stage)
	}
}

// searchAgainst executes the search SQL against the named FTS5 table.
// Caller is responsible for ensuring `index` is one of the package
// constants; we never interpolate user input here.
func searchAgainst(ctx context.Context, db *sql.DB, opts SearchEventOpts, index string) ([]SearchEventHit, error) {
	sqlText, args := buildSearchSQL(opts, index)
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("SearchEvents: query (%s): %w", index, err)
	}
	defer func() { _ = rows.Close() }()

	var hits []SearchEventHit
	for rows.Next() {
		h, err := scanSearchEventHit(rows)
		if err != nil {
			return nil, fmt.Errorf("SearchEvents: scan: %w", err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SearchEvents: iterate: %w", err)
	}
	return hits, nil
}

// scanSearchEventHit scans one row in the 6-column projection used
// by both the FTS path (searchAgainst) and the extractions path
// (searchExtractions). The Cwd / Content / Snippet columns are
// nullable; everything else is required.
func scanSearchEventHit(rows *sql.Rows) (SearchEventHit, error) {
	var (
		h       SearchEventHit
		cwd     sql.NullString
		content sql.NullString
		snippet sql.NullString
	)
	if err := rows.Scan(&h.SessionID, &h.Kind, &cwd, &h.TsSourceMs, &content, &snippet); err != nil {
		return SearchEventHit{}, err
	}
	h.Cwd = nullable.StringPtr(cwd)
	h.Content = nullable.StringPtr(content)
	h.Snippet = nullable.StringPtr(snippet)
	return h, nil
}

// appendCommonFilters writes the shared scalar / facet filter
// clauses (kind, session_id, subagent_id, since_ms, source_agent,
// tool_name, skill_name, file_path, with_failures) into filter
// and appends the matching bind args. Used by both buildSearchSQL
// and searchExtractions so a new facet only has to be added once.
func appendCommonFilters(filter *strings.Builder, args *[]any, opts SearchEventOpts) {
	if opts.Kind != "" {
		filter.WriteString(` AND e.kind = ?`)
		*args = append(*args, opts.Kind)
	}
	if opts.SessionID != "" {
		filter.WriteString(` AND e.session_id = ?`)
		*args = append(*args, opts.SessionID)
	}
	if opts.SubagentID != "" {
		filter.WriteString(` AND e.subagent_id = ?`)
		*args = append(*args, opts.SubagentID)
	}
	if opts.SinceMs > 0 {
		filter.WriteString(` AND e.ts_source_ms >= ?`)
		*args = append(*args, opts.SinceMs)
	}
	if opts.SourceAgent != "" {
		filter.WriteString(` AND e.source_agent = ?`)
		*args = append(*args, opts.SourceAgent)
	}
	if opts.ToolName != "" {
		filter.WriteString(` AND e.tool_name = ?`)
		*args = append(*args, opts.ToolName)
	}
	if opts.SkillName != "" {
		// Session-level: every event in a session where the
		// named skill loaded. The (kind, value) pair is covered
		// by idx_extractions_kind_value so this stays cheap.
		filter.WriteString(` AND e.session_id IN (
			SELECT session_id FROM extractions WHERE kind = ? AND value = ?
		)`)
		*args = append(*args, events.ExtractionKindSkillLoad, opts.SkillName)
	}
	if opts.FilePathSubstring != "" {
		// Session-level + LIKE %substring% so a partial path
		// ("migrate.go", "internal/store") matches. file_path
		// extractions are the canonical source — see
		// internal/events/FilePathExtractor.
		filter.WriteString(` AND e.session_id IN (
			SELECT session_id FROM extractions WHERE kind = ? AND value LIKE ?
		)`)
		*args = append(*args, events.ExtractionKindFilePath, "%"+opts.FilePathSubstring+"%")
	}
	if opts.WithFailures {
		filter.WriteString(` AND e.session_id IN (
			SELECT session_id FROM events WHERE kind = ?
		)`)
		*args = append(*args, events.KindToolFailure)
	}
}

// buildSearchSQL composes the SQL + bind args for one SearchEvents
// call against the named FTS5 virtual table. The index argument is
// interpolated as a SQL identifier; callers must pass a package
// constant (indexPrimary or indexTrigram), never user input.
func buildSearchSQL(opts SearchEventOpts, index string) (string, []any) {
	var filter strings.Builder
	args := []any{opts.Query}

	appendCommonFilters(&filter, &args, opts)

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}

	// snippet() centers on the matched terms, ≤ snippetTokens tokens,
	// with `…` ellipsis on either side when truncation occurs. Empty
	// delimiters keep the output compatible with the existing
	// table/JSON formats (callers can wrap the snippet themselves).
	// First arg must be the FTS5 table identifier — interpolated
	// from `index`, never user input.
	const snippetTokens = 16
	snippetExpr := fmt.Sprintf(
		`snippet(%s, 0, '', '', '…', %d)`, index, snippetTokens,
	)

	if opts.NoDedup {
		// Bare path. f.rank is the FTS5 bm25 score (lower-is-better).
		// For OrderRank we divide by pow(2, days_old / recencyHalfLifeDays)
		// so older rows drift toward zero without flipping sign.
		// `?` for now_ms is appended to args before LIMIT.
		var order string
		switch opts.Order {
		case OrderRecency:
			order = "e.ts_source_ms DESC"
		default:
			order = recencyBoostedRankExpr("f.rank", "e.ts_source_ms")
			args = append(args, opts.NowMs)
		}
		sqlText := `SELECT e.session_id, e.kind, e.cwd, e.ts_source_ms, e.content_text,
				` + snippetExpr + ` AS snip
			FROM ` + index + ` f JOIN events e ON e.rowid = f.rowid
			WHERE ` + index + ` MATCH ?` + filter.String() + `
			ORDER BY ` + order + `, e.rowid DESC LIMIT ? OFFSET ?`
		args = append(args, limit, opts.Offset)
		return sqlText, args
	}

	// Deduped path. COALESCE on content_text so NULL partitions don't
	// collapse into a single group. Kind is included so tool_use and
	// assistant_message of a turn stay distinct even if they share
	// (session_id, role, content). Within a partition, hook beats
	// import; FTS rank then rowid break ties.
	//
	// Outer ORDER BY uses the recency-boosted bm25 score for
	// OrderRank, or pure ts_source_ms for OrderRecency.
	var order string
	switch opts.Order {
	case OrderRecency:
		order = "ts_source_ms DESC"
	default:
		order = recencyBoostedRankExpr("fts_rank", "ts_source_ms")
	}
	sqlText := `WITH matched AS (
			SELECT e.rowid, e.session_id, e.role, e.kind, e.cwd,
				e.ts_source_ms, e.content_text, e.source_agent,
				(CASE WHEN e.transport = 'hook' THEN 0 ELSE 1 END) AS transport_rank,
				f.rank AS fts_rank,
				` + snippetExpr + ` AS snip
			FROM ` + index + ` f
			JOIN events e ON e.rowid = f.rowid
			WHERE ` + index + ` MATCH ?` + filter.String() + `
		),
		ranked AS (
			SELECT *,
				ROW_NUMBER() OVER (
					PARTITION BY session_id, role, kind, COALESCE(content_text, rowid)
					ORDER BY transport_rank, fts_rank, rowid
				) AS rn
			FROM matched
		)
		SELECT session_id, kind, cwd, ts_source_ms, content_text, snip
		FROM ranked
		WHERE rn = 1
		ORDER BY ` + order + `, rowid DESC
		LIMIT ? OFFSET ?`
	if opts.Order != OrderRecency {
		// Boosted formula has a `?` for now_ms; matches the position
		// in the SELECT clause's ORDER BY.
		args = append(args, opts.NowMs)
	}
	args = append(args, limit, opts.Offset)
	return sqlText, args
}

// searchExtractions runs MATCH against extractions_fts and synthesises
// SearchEventHit rows by joining the matching extractions back to
// their events. The snippet is labelled with the extraction kind so
// the caller knows the hit came via a typed fact rather than message
// text — e.g. `[file_path] internal/store/migrate.go`.
//
// Multiple extractions on the same event collapse to one row (best
// fts_rank wins). Filters (kind, session_id, since_ms, limit) and
// Order behave the same as the events-FTS path.
func searchExtractions(ctx context.Context, db *sql.DB, opts SearchEventOpts) ([]SearchEventHit, error) {
	var filter strings.Builder
	args := []any{opts.Query}

	appendCommonFilters(&filter, &args, opts)

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}

	var order string
	switch opts.Order {
	case OrderRecency:
		order = "e.ts_source_ms DESC"
	default:
		order = recencyBoostedRankExpr("fts_rank", "e.ts_source_ms")
	}

	sqlText := `WITH ext_matched AS (
			SELECT x.event_id, x.kind AS ext_kind, x.value AS ext_value,
				ef.rank AS fts_rank
			FROM extractions_fts ef
			JOIN extractions x ON x.rowid = ef.rowid
			WHERE extractions_fts MATCH ?
		),
		event_picked AS (
			SELECT event_id, ext_kind, ext_value, fts_rank,
				ROW_NUMBER() OVER (PARTITION BY event_id ORDER BY fts_rank) AS rn
			FROM ext_matched
		)
		SELECT e.session_id, e.kind, e.cwd, e.ts_source_ms, e.content_text,
			'[' || ep.ext_kind || '] ' || ep.ext_value AS snip
		FROM event_picked ep
		JOIN events e ON e.event_id = ep.event_id
		WHERE ep.rn = 1` + filter.String() + `
		ORDER BY ` + order + `, e.rowid DESC
		LIMIT ? OFFSET ?`

	if opts.Order != OrderRecency {
		args = append(args, opts.NowMs)
	}
	args = append(args, limit, opts.Offset)

	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("SearchEvents: extractions query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []SearchEventHit
	for rows.Next() {
		h, err := scanSearchEventHit(rows)
		if err != nil {
			return nil, fmt.Errorf("SearchEvents: extractions scan: %w", err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SearchEvents: extractions iterate: %w", err)
	}
	return hits, nil
}
