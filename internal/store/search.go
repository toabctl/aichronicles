package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SearchOrder selects how SearchEvents orders its result rows.
type SearchOrder int

const (
	// OrderRank sorts by FTS5 relevance (most relevant first).
	// Default for the CLI's interactive search.
	OrderRank SearchOrder = iota

	// OrderRecency sorts by ts_source_ms DESC (newest first).
	// Default for the MCP search_events tool, where an agent asking
	// "did I work on X recently?" wants chronological order.
	OrderRecency
)

// SearchEventOpts is the input contract for SearchEvents.
//
// Query must already be a syntactically-valid FTS5 MATCH expression;
// pass it through internal/searchquery.ToFTS5 first if it came from
// human or agent input.
type SearchEventOpts struct {
	Query     string
	Kind      string
	SessionID string
	SinceMs   int64
	Limit     int
	NoDedup   bool
	Order     SearchOrder
}

// SearchEventHit is one row returned by SearchEvents. Cwd and Content
// are nullable to mirror the underlying schema; callers decide how to
// render NULLs for their format.
type SearchEventHit struct {
	SessionID  string
	Kind       string
	Cwd        sql.NullString
	TsSourceMs int64
	Content    sql.NullString
}

// FTS index names. Two virtual tables shadow events.content_text:
//   - events_fts uses unicode61 with code-friendly separators; this
//     is the primary path for whole-word and identifier-aware queries.
//   - events_fts_trigram uses the trigram tokenizer for substring
//     matches, consulted only when the primary returns nothing.
const (
	indexPrimary = "events_fts"
	indexTrigram = "events_fts_trigram"
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
	if strings.TrimSpace(opts.Query) == "" {
		return nil, fmt.Errorf("SearchEvents: query is required")
	}
	hits, err := searchAgainst(ctx, db, opts, indexPrimary)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		hits, err = searchAgainst(ctx, db, opts, indexTrigram)
		if err != nil {
			return nil, err
		}
	}
	return hits, nil
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
		var h SearchEventHit
		if err := rows.Scan(&h.SessionID, &h.Kind, &h.Cwd, &h.TsSourceMs, &h.Content); err != nil {
			return nil, fmt.Errorf("SearchEvents: scan: %w", err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SearchEvents: iterate: %w", err)
	}
	return hits, nil
}

// buildSearchSQL composes the SQL + bind args for one SearchEvents
// call against the named FTS5 virtual table. The index argument is
// interpolated as a SQL identifier; callers must pass a package
// constant (indexPrimary or indexTrigram), never user input.
func buildSearchSQL(opts SearchEventOpts, index string) (string, []any) {
	var filter strings.Builder
	args := []any{opts.Query}

	if opts.Kind != "" {
		filter.WriteString(` AND e.kind = ?`)
		args = append(args, opts.Kind)
	}
	if opts.SessionID != "" {
		filter.WriteString(` AND e.session_id = ?`)
		args = append(args, opts.SessionID)
	}
	if opts.SinceMs > 0 {
		filter.WriteString(` AND e.ts_source_ms >= ?`)
		args = append(args, opts.SinceMs)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}

	if opts.NoDedup {
		// Bare path: f.rank is the FTS5 special column.
		order := "rank"
		if opts.Order == OrderRecency {
			order = "ts_source_ms DESC"
		}
		sqlText := `SELECT e.session_id, e.kind, e.cwd, e.ts_source_ms, e.content_text
			FROM ` + index + ` f JOIN events e ON e.rowid = f.rowid
			WHERE ` + index + ` MATCH ?` + filter.String() + `
			ORDER BY ` + order + ` LIMIT ?`
		args = append(args, limit)
		return sqlText, args
	}

	// Deduped path. COALESCE on content_text so NULL partitions don't
	// collapse into a single group. Kind is included so tool_use and
	// assistant_message of a turn stay distinct even if they share
	// (session_id, role, content). Within a partition, hook beats
	// import; FTS rank then rowid break ties.
	//
	// Outer ORDER BY references fts_rank (alias from the CTE) for
	// rank ordering, or ts_source_ms for recency.
	order := "fts_rank"
	if opts.Order == OrderRecency {
		order = "ts_source_ms DESC"
	}
	sqlText := `WITH matched AS (
			SELECT e.rowid, e.session_id, e.role, e.kind, e.cwd,
				e.ts_source_ms, e.content_text, e.source_agent,
				(CASE
					WHEN json_extract(r.envelope_json, '$.transport') = 'hook'
					THEN 0 ELSE 1
				END) AS transport_rank,
				f.rank AS fts_rank
			FROM ` + index + ` f
			JOIN events e         ON e.rowid = f.rowid
			JOIN raw_envelopes r  ON r.event_id = e.event_id
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
		SELECT session_id, kind, cwd, ts_source_ms, content_text
		FROM ranked
		WHERE rn = 1
		ORDER BY ` + order + `
		LIMIT ?`
	args = append(args, limit)
	return sqlText, args
}
