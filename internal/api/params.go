package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/toabctl/aichronicles/internal/wire"
)

// parseInt64Query reads an optional non-negative int64 query
// parameter. Returns (0, true) when missing, the parsed value
// when valid, or (0, false) after writing a 400 problem response.
// Callers must return immediately when ok is false.
//
// Canonical shape for since_ms / since_seq / window_ms style
// cursors and windows: present-or-absent, never negative, no
// other constraints. The 400 detail is left empty; the title
// ("Invalid <name>") is enough signal for the API contract.
func parseInt64Query(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid "+name, "")
		return 0, false
	}
	return n, true
}

// parseLimitQuery reads the optional "limit" query parameter,
// capped at wire.MaxPageLimit. Returns (def, true) when missing,
// (n, true) when valid (capped at MaxPageLimit), or (0, false)
// after writing a 400 problem response. Callers must return
// immediately when ok is false.
//
// Limit must be strictly positive — zero and negative values 400.
// MaxPageLimit is clamped silently (not 400) so callers can ask
// for "as many as possible" without coupling to the exact cap.
func parseLimitQuery(w http.ResponseWriter, r *http.Request, def int) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
		return 0, false
	}
	if n > wire.MaxPageLimit {
		return wire.MaxPageLimit, true
	}
	return n, true
}

// parsePositiveIntQuery reads an optional positive int query
// parameter with no upper cap. Returns (def, true) when missing,
// (n, true) when valid, or (0, false) after writing a 400 problem.
// Used for top_tools / top_skills / top_sessions / max_skills /
// max_examples style limits where there's no shared MaxPageLimit
// cap.
func parsePositiveIntQuery(w http.ResponseWriter, r *http.Request, name string, def int) (int, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid "+name, "")
		return 0, false
	}
	return n, true
}

// parseSessionIDsQuery splits a comma-separated session_ids query
// value into a deduplicated slice. Empty input returns nil so the
// caller can detect "no ids" without a separate length check.
// Whitespace around each id is trimmed; empty splits drop out.
// Order is preserved (first occurrence wins) — useful when a
// renderer wants to show results in the same order the client
// asked.
func parseSessionIDsQuery(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseNonNegativeIntQuery reads an optional non-negative int query
// parameter where 0 is a meaningful value (typically "no minimum"
// or "no upper bound"). Returns (def, true) when missing, (n, true)
// when valid (≥ 0), or (0, false) after writing a 400 problem
// response. Callers must return immediately when ok is false.
//
// Distinct from parsePositiveIntQuery (which rejects 0) because
// "?limit=0" on the audit / discovery-read endpoints legitimately
// means "no LIMIT clause" rather than an ambiguous request.
func parseNonNegativeIntQuery(w http.ResponseWriter, r *http.Request, name string, def int) (int, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid "+name, "")
		return 0, false
	}
	return n, true
}
