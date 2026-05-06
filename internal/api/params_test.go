package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseInt64Query(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		query     string
		wantVal   int64
		wantOk    bool
		wantCode  int
		wantTitle string
	}{
		{name: "missing", query: "", wantVal: 0, wantOk: true},
		{name: "valid zero", query: "since_ms=0", wantVal: 0, wantOk: true},
		{name: "valid positive", query: "since_ms=12345", wantVal: 12345, wantOk: true},
		{name: "negative", query: "since_ms=-1", wantOk: false, wantCode: http.StatusBadRequest, wantTitle: "Invalid since_ms"},
		{name: "non-numeric", query: "since_ms=abc", wantOk: false, wantCode: http.StatusBadRequest, wantTitle: "Invalid since_ms"},
		{name: "overflow", query: "since_ms=99999999999999999999", wantOk: false, wantCode: http.StatusBadRequest, wantTitle: "Invalid since_ms"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x?"+tc.query, nil)
			got, ok := parseInt64Query(rr, req, "since_ms")
			if got != tc.wantVal {
				t.Errorf("val: got %d, want %d", got, tc.wantVal)
			}
			if ok != tc.wantOk {
				t.Errorf("ok: got %v, want %v", ok, tc.wantOk)
			}
			if !tc.wantOk && rr.Code != tc.wantCode {
				t.Errorf("status: got %d, want %d", rr.Code, tc.wantCode)
			}
			if !tc.wantOk && tc.wantTitle != "" && !strings.Contains(rr.Body.String(), tc.wantTitle) {
				t.Errorf("body %q missing title %q", rr.Body.String(), tc.wantTitle)
			}
		})
	}
}
