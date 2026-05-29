package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/wire"
)

func TestParsePage(t *testing.T) {
	t.Parallel()

	mustCursor := func(off int) string {
		c, err := wire.EncodePageCursor(wire.PageCursor{Off: off})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return string(c)
	}

	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
		wantOK     bool
	}{
		{name: "defaults", query: "", wantLimit: wire.DefaultPageLimit, wantOffset: 0, wantOK: true},
		{name: "explicit limit", query: "limit=5", wantLimit: 5, wantOffset: 0, wantOK: true},
		{name: "cursor sets offset", query: "limit=5&cursor=" + mustCursor(15), wantLimit: 5, wantOffset: 15, wantOK: true},
		{name: "limit over cap clamps", query: "limit=999999", wantLimit: wire.MaxPageLimit, wantOffset: 0, wantOK: true},
		{name: "bad limit 400", query: "limit=0", wantOK: false},
		{name: "malformed cursor 400", query: "cursor=%21%21not-base64", wantOK: false},
		{name: "too deep 400", query: "limit=10&cursor=" + mustCursor(wire.MaxOffset), wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/x?"+tc.query, nil)
			limit, offset, ok := parsePage(rr, r)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v, want %v (status=%d)", ok, tc.wantOK, rr.Code)
			}
			if !ok {
				if rr.Code != http.StatusBadRequest {
					t.Errorf("expected 400, got %d", rr.Code)
				}
				return
			}
			if limit != tc.wantLimit || offset != tc.wantOffset {
				t.Errorf("got (limit=%d, offset=%d), want (limit=%d, offset=%d)",
					limit, offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

func TestNextCursor(t *testing.T) {
	t.Parallel()
	// Short page → no next cursor.
	if c := nextCursor(0, 50, 12); c != "" {
		t.Errorf("short page should yield empty cursor, got %q", c)
	}
	// Full page → cursor decodes to offset+returned.
	c := nextCursor(10, 5, 5)
	if c == "" {
		t.Fatal("full page should yield a cursor")
	}
	got, err := wire.DecodePageCursor(c)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Off != 15 {
		t.Errorf("next offset: got %d, want 15", got.Off)
	}
}

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

// TestDecodeJSONBody_RejectsOversizedPayload pins the
// MaxJSONBodyBytes cap: a chunked-transfer / streamed POST whose
// body exceeds the cap returns 413 rather than streaming gigabytes
// into json.Decoder. Without the wrap, a malicious or buggy client
// could OOM the daemon despite the small struct shape on the
// server side.
func TestDecodeJSONBody_RejectsOversizedPayload(t *testing.T) {
	t.Parallel()

	// Build a body just over the cap. The content is a
	// well-formed JSON string with a giant filler so the decoder
	// would otherwise allocate proportionally.
	overSize := MaxJSONBodyBytes + 1024
	body := make([]byte, 0, overSize+32)
	body = append(body, []byte(`{"name":"`)...)
	body = append(body, bytes.Repeat([]byte("a"), overSize)...)
	body = append(body, []byte(`"}`)...)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))

	var dst struct {
		Name string `json:"name"`
	}
	if ok := decodeJSONBody(rr, req, &dst); ok {
		t.Fatalf("decodeJSONBody should have refused oversize body")
	}
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusRequestEntityTooLarge)
	}
	if !strings.Contains(rr.Body.String(), "Payload too large") {
		t.Errorf("body missing 413 title: %q", rr.Body.String())
	}
}
