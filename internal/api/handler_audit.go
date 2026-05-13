package api

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/toabctl/aichronicles/internal/nullable"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/wire"
)

// auditSnippetRunes caps the per-row snippet returned by /v1/audit.
// Match the legacy CLI cap so the wire shape stays scannable.
const auditSnippetRunes = 120

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
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	limit, ok := parseNonNegativeIntQuery(w, r, "limit", 0)
	if !ok {
		return
	}

	sqlText, args := buildAuditQuery(sinceMs, limit)
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
			Snippet:    auditSnippet(content.String, findings[0]),
		})
	}
	if err := rows.Err(); err != nil {
		s.storeError(w, "audit rows.Err", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
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
// finding so the operator can see where the match occurred. The
// matched bytes are replaced with the marker form so the wire
// payload never carries raw secrets — copy-pasting an audit
// response into a ticket is safe.
func auditSnippet(content string, f redact.Finding) string {
	start, end := f.Start, f.End
	if start < 0 {
		start = 0
	}
	if end > len(content) {
		end = len(content)
	}
	prefix := content[:start]
	suffix := content[end:]
	pre := []rune(prefix)
	post := []rune(suffix)
	padding := auditSnippetRunes / 2
	if len(pre) > padding {
		pre = append([]rune{'…'}, pre[len(pre)-padding:]...)
	}
	if len(post) > padding {
		post = append(post[:padding], '…')
	}
	hit := []rune(content[start:end])
	combined := string(pre) + string(hit) + string(post)
	combined = strings.ReplaceAll(combined, "\n", " ")
	combined = strings.ReplaceAll(combined, "\r", " ")
	combined = strings.ReplaceAll(combined, "\t", " ")
	if r := []rune(combined); len(r) > auditSnippetRunes {
		combined = string(r[:auditSnippetRunes]) + "…"
	}
	return strings.Replace(combined, string(hit), "<"+f.Pattern+">", 1)
}
