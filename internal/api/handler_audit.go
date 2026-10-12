package api

import (
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/toabctl/aichronicles/internal/nullable"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/wire"
)

// auditSnippetRunes caps the per-row snippet returned by /v1/audit.
// Match the legacy CLI cap so the wire shape stays scannable.
const auditSnippetRunes = 120

// auditMaxRowsCeiling is the hard upper bound on rows scanned per
// /v1/audit call, regardless of what the client passes. limit=0 or
// absent means "the ceiling". A full-table scan in one request would
// push hundreds of MB of content through the regex scanner while
// holding a read transaction open; instead a client that wants every
// row follows next_cursor page by page.
const auditMaxRowsCeiling = 5000

// handleAudit serves GET /v1/audit. Walks events.content_text and
// runs redact.Default() against every non-null row, returning one
// finding per matched event plus aggregate counters for the page.
//
// Query params (all optional):
//   - since_ms: only scan events with ts_source_ms >= since_ms
//   - limit:    page size (rows scanned), capped at auditMaxRowsCeiling
//   - cursor:   next_cursor from the previous page
//
// Rows are scanned most-recently-ingested first, keyset-paginated on
// raw_envelopes.ingest_seq (see wire.AuditCursor): each page is an
// index range scan, so paging through the whole table costs one pass
// in total rather than one sort per page. next_cursor is set whenever
// the page came back full; an empty next_cursor is the only signal
// that the scan reached the end.
//
// Server-side scan: the pattern set is the same one the ingest
// pipeline uses, so this is the canonical "what would the redactor
// catch right now" check. Raw secret bytes never leave the server —
// the snippet field always carries the marker form.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	clearWriteDeadlineForLongOp(w)
	req, beforeSeq, ok := parseAuditRequest(w, r)
	if !ok {
		return
	}

	sqlText, args := buildAuditQuery(req.SinceMs, beforeSeq, req.Limit)
	rows, err := s.store.DB().QueryContext(r.Context(), sqlText, args...)
	if err != nil {
		s.storeError(w, "audit query", err)
		return
	}
	defer func() { _ = rows.Close() }()

	scanner := redact.Default()
	resp := wire.AuditResponse{
		Findings:    make([]wire.AuditFinding, 0, 8),
		PatternHits: map[string]int{},
	}
	var lastSeq int64
	for rows.Next() {
		var (
			seq     int64
			sess    string
			tsMs    sql.NullInt64
			kind    string
			content sql.NullString
		)
		if err := rows.Scan(&seq, &sess, &tsMs, &kind, &content); err != nil {
			s.storeError(w, "audit scan", err)
			return
		}
		lastSeq = seq
		resp.Scanned++
		if !content.Valid || content.String == "" {
			continue
		}
		findings := scanner.Scan(content.String)
		if len(findings) == 0 {
			continue
		}
		resp.Flagged++
		resp.TotalFindings += len(findings)

		names := uniquePatternNames(findings)
		for _, n := range names {
			resp.PatternHits[n]++
		}

		resp.Findings = append(resp.Findings, wire.AuditFinding{
			SessionID:  sess,
			TsSourceMs: nullable.Int64Ptr(tsMs),
			Kind:       kind,
			Patterns:   names,
			Snippet:    auditSnippet(content.String, findings),
		})
	}
	if err := rows.Err(); err != nil {
		s.storeError(w, "audit rows.Err", err)
		return
	}
	// A full page means rows may remain; a short page is the end. The
	// same stop rule as nextCursor, keyed on ingest_seq instead of an
	// offset. Encoding a single-int payload can't fail.
	if resp.Scanned == req.Limit {
		resp.NextCursor, _ = wire.EncodeAuditCursor(wire.AuditCursor{BeforeSeq: lastSeq})
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseAuditRequest decodes + validates the GET /v1/audit query into
// wire.AuditRequest (server mirror of apiclient.Client.Audit) plus the
// decoded keyset position (0 = first page). Limit 0 ("missing") means
// "use the ceiling", and anything above it clamps down so an operator
// passing limit=99999 doesn't strand the daemon in a
// redact-everything pass. A malformed cursor, or one that doesn't
// address a positive ingest_seq, is a 400 rather than a silent
// restart from the top.
func parseAuditRequest(w http.ResponseWriter, r *http.Request) (wire.AuditRequest, int64, bool) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return wire.AuditRequest{}, 0, false
	}
	limit, ok := parseNonNegativeIntQuery(w, r, "limit", 0)
	if !ok {
		return wire.AuditRequest{}, 0, false
	}
	if limit <= 0 || limit > auditMaxRowsCeiling {
		limit = auditMaxRowsCeiling
	}
	req := wire.AuditRequest{SinceMs: sinceMs, Limit: limit}
	var beforeSeq int64
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cur, err := wire.DecodeAuditCursor(wire.Cursor(raw))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "Invalid cursor", err.Error())
			return wire.AuditRequest{}, 0, false
		}
		if cur.BeforeSeq <= 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid cursor",
				"cursor does not address an ingest_seq")
			return wire.AuditRequest{}, 0, false
		}
		req.Cursor = wire.Cursor(raw)
		beforeSeq = cur.BeforeSeq
	}
	return req, beforeSeq, true
}

// buildAuditQuery composes one audit page: events with non-null
// content_text, most-recently-ingested first, optional since_ms
// cutoff, optional keyset position (beforeSeq > 0) and a row limit.
// Driven by raw_envelopes' unique ingest_seq index; every event has
// its envelope (events.event_id REFERENCES raw_envelopes), so the
// inner join drops nothing. Inline rather than living in
// internal/store because audit is a redact-driven server-side
// operation; keeping the SQL next to the handler makes the data-flow
// obvious.
func buildAuditQuery(sinceMs, beforeSeq int64, limit int) (string, []any) {
	var filter strings.Builder
	var args []any
	if sinceMs > 0 {
		filter.WriteString(` AND e.ts_source_ms >= ?`)
		args = append(args, sinceMs)
	}
	if beforeSeq > 0 {
		filter.WriteString(` AND r.ingest_seq < ?`)
		args = append(args, beforeSeq)
	}
	q := `SELECT r.ingest_seq, e.session_id, e.ts_source_ms, e.kind, e.content_text
		FROM raw_envelopes r
		JOIN events e ON e.event_id = r.event_id
		WHERE e.content_text IS NOT NULL` + filter.String() + `
		ORDER BY r.ingest_seq DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return q, args
}

func uniquePatternNames(findings []redact.Finding) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		if _, ok := seen[f.Pattern]; ok {
			continue
		}
		seen[f.Pattern] = struct{}{}
		out = append(out, f.Pattern)
	}
	return out
}

// auditSnippet renders a short context window around the first
// finding so the operator can see where the match occurred. EVERY
// finding in the row is replaced with its marker form before the
// window is cut, so the wire payload never carries raw secret bytes —
// copy-pasting an audit response into a ticket is safe even when a
// row holds several secrets side by side.
func auditSnippet(content string, findings []redact.Finding) string {
	masked, anchorStart, anchorEnd := maskFindings(content, findings)

	// The window is cut from the fully-masked text, never from the raw
	// content. Windowing the raw content around the first hit (as this
	// once did) leaked every other secret inside the ±padding context;
	// building the buffer around a raw hit and replacing it at the end
	// (as it did before that) leaked the hit itself once the
	// whitespace rewrites or the rune cap below mutated the buffer.
	marker := masked[anchorStart:anchorEnd]
	budget := auditSnippetRunes - utf8.RuneCountInString(marker)
	if budget < 0 {
		budget = 0
	}
	padding := budget / 2

	pre := []rune(masked[:anchorStart])
	post := []rune(masked[anchorEnd:])
	if len(pre) > padding {
		pre = append([]rune{'…'}, pre[len(pre)-padding:]...)
	}
	if len(post) > padding {
		post = append(post[:padding], '…')
	}

	combined := string(pre) + marker + string(post)
	combined = strings.ReplaceAll(combined, "\n", " ")
	combined = strings.ReplaceAll(combined, "\r", " ")
	combined = strings.ReplaceAll(combined, "\t", " ")
	// Safety net only. The buffer already carries no raw secret, so a
	// tail trim can cost context but never confidentiality; the
	// padding budget above keeps the marker itself inside the cap.
	if r := []rune(combined); len(r) > auditSnippetRunes {
		combined = string(r[:auditSnippetRunes]) + "…"
	}
	return combined
}

// maskFindings returns content with every finding's bytes replaced by
// its "<pattern>" marker, plus the byte span of the first finding's
// marker in the result (the snippet anchor).
//
// Findings are defended rather than trusted: offsets are clamped into
// range, the list is sorted by Start, and a finding that overlaps an
// earlier one extends the masked region instead of being dropped —
// erring toward masking too much, never too little. With no usable
// finding the anchor is an empty span at offset 0.
func maskFindings(content string, findings []redact.Finding) (masked string, anchorStart, anchorEnd int) {
	local := make([]redact.Finding, len(findings))
	for i, f := range findings {
		f.Start = max(f.Start, 0)
		f.End = min(f.End, len(content))
		if f.Start > f.End {
			f.Start = f.End
		}
		local[i] = f
	}
	sort.SliceStable(local, func(i, j int) bool { return local[i].Start < local[j].Start })

	var b strings.Builder
	b.Grow(len(content))
	anchored := false
	prev := 0
	for _, f := range local {
		if f.Start < prev {
			// Overlaps the previous masked span; its leading bytes are
			// already hidden. Hide the tail too, under the same marker.
			prev = max(prev, f.End)
			continue
		}
		b.WriteString(content[prev:f.Start])
		if !anchored {
			anchorStart = b.Len()
		}
		b.WriteString("<" + f.Pattern + ">")
		if !anchored {
			anchorEnd = b.Len()
			anchored = true
		}
		prev = f.End
	}
	b.WriteString(content[prev:])
	return b.String(), anchorStart, anchorEnd
}
