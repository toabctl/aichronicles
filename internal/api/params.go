package api

import (
	"net/http"
	"strconv"

	"github.com/toabctl/aichronicles/pkg/api"
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
// capped at api.MaxPageLimit. Returns (def, true) when missing,
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
	if n > api.MaxPageLimit {
		return api.MaxPageLimit, true
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

// positiveOrZero parses "" → 0, valid non-negative int → that
// int, or -1 to signal "the caller should respond with 400".
// Used by handlers that want a soft "0 = use default, -1 =
// reject" handshake without forcing a writeProblem inside the
// helper (the caller writes its own message naming the param).
func positiveOrZero(v string) int {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return -1
	}
	return n
}
