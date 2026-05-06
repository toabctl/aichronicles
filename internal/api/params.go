package api

import (
	"net/http"
	"strconv"
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
