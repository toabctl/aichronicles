package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestParseSessionIDsQuery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: "", want: nil},
		{name: "single", in: "abc", want: []string{"abc"}},
		{name: "comma list", in: "a,b,c", want: []string{"a", "b", "c"}},
		{name: "trims whitespace", in: " a , b ,c ", want: []string{"a", "b", "c"}},
		{name: "dedupes first wins", in: "a,b,a,c,b", want: []string{"a", "b", "c"}},
		{name: "only commas", in: ",,,", want: nil},
		{name: "only whitespace", in: "  ,  ,", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseSessionIDsQuery(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

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
