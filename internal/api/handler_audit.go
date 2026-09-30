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
// /v1/audit call, regardless of what the client passes. limit=0
// (the default from parseNonNegativeIntQuery) used to mean
// "no LIMIT clause" and triggered a full ORDER BY ts_source_ms DESC
// scan of every row in `events` through the redact scanner — on a
// real corpus that is hundreds of MB of regex work plus SQLite
// write-lock contention. Now it means "the ceiling"; a client that
// wants more rows must page via since_ms.
const auditMaxRowsCeiling = 5000

// handleAudit serves GET /v1/audit. Walks events.content_text and
// runs redact.Default() against every non-null row, returning one
// finding per matched event plus aggregate counters.
//
// Query params (all optional):
//   - since_ms: only scan events with ts_source_ms >= since_ms
//   - limit:    cap on rows scanned (newest first)
//
// Server-side scan: the pattern set is the same one the ingest
// pipeline uses, so this is the canonical "what would the redactor
// catch right now" check. Raw secret bytes never leave the server —
// the snippet field always carries the marker form.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	clearWriteDeadlineForLongOp(w)
	req, ok := parseAuditRequest(w, r)
	if !ok {
		return
	}

	sqlText, args := buildAuditQuery(req.SinceMs, req.Limit)
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
	for rows.Next() {
		var (
			sess    string
			tsMs    sql.NullInt64
			kind    string
			content sql.NullString
		)
		if err := rows.Scan(&sess, &tsMs, &kind, &content); err != nil {
			s.storeError(w, "audit scan", err)
			return
		}
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
	writeJSON(w, http.StatusOK, resp)
}

// parseAuditRequest decodes + validates the GET /v1/audit query into
// wire.AuditRequest (server mirror of apiclient.Client.Audit). The
// Limit returned is already clamped to the server-side ceiling: 0
// ("missing") means "use the ceiling", and anything above it clamps
// down so an operator passing limit=99999 doesn't strand the daemon
// in a redact-everything pass.
func parseAuditRequest(w http.ResponseWriter, r *http.Request) (wire.AuditRequest, bool) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return wire.AuditRequest{}, false
	}
	limit, ok := parseNonNegativeIntQuery(w, r, "limit", 0)
	if !ok {
		return wire.AuditRequest{}, false
	}
	if limit <= 0 || limit > auditMaxRowsCeiling {
		limit = auditMaxRowsCeiling
	}
	return wire.AuditRequest{SinceMs: sinceMs, Limit: limit}, true
}

// buildAuditQuery composes the audit scan query: every event with
// non-null content_text, newest-first, optional since_ms cutoff and
// row limit. Inline rather than living in internal/store because
// audit is a redact-driven server-side operation; keeping the
// SQL next to the handler makes the data-flow obvious.
func buildAuditQuery(sinceMs int64, limit int) (string, []any) {
	var filter strings.Builder
	var args []any
	if sinceMs > 0 {
		filter.WriteString(` AND ts_source_ms >= ?`)
		args = append(args, sinceMs)
	}
	q := `SELECT session_id, ts_source_ms, kind, content_text
		FROM events
		WHERE content_text IS NOT NULL` + filter.String() + `
		ORDER BY ts_source_ms DESC`
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
