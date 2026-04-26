package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSearchPage_RendersForm(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/search")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	for _, want := range []string{
		`<input type="search"`,      // the search input
		`hx-get="/search/hits"`,     // htmx wiring
		`hx-trigger="input changed`, // type-as-you-search
		"start typing",              // empty-state inside #hits
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestSearchHits_EmptyQueryRendersEmptyState(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/search/hits?q=")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	// Empty query → empty SearchHits → template falls through to
	// the empty-state line (no error, no rows).
	if !strings.Contains(body, "(no hits for that query)") {
		t.Errorf("expected empty-hits line:\n%s", body)
	}
}

func TestSearchHits_BareTokenMatchesByPrefix(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	seedSession(t, st, "sess-fts", "how to parse jsonl files in Go", now)

	base, stop := startTestServer(t, st)
	defer stop()

	// `json` becomes `json*` via searchquery.ToFTS5 and matches
	// `jsonl` — proves the same parser the CLI / MCP use is
	// reachable from the web layer.
	status, body := fetch(t, base+"/search/hits?"+url.Values{"q": {"json"}}.Encode())
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", status, body)
	}
	if !strings.Contains(body, "jsonl") {
		t.Errorf("expected snippet to contain `jsonl`:\n%s", body)
	}
	if !strings.Contains(body, `<a href="/sessions/`) {
		t.Errorf("hit row should link to session detail:\n%s", body)
	}
}

func TestSearchHits_UnclosedQuoteSurfacesAsError(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+`/search/hits?q=`+url.QueryEscape(`open "and then`))
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	// The fragment renders the parser's error inline so the user
	// sees a corrective hint instead of an opaque server-error.
	if !strings.Contains(body, "unclosed quote") {
		t.Errorf("expected unclosed-quote diagnostic:\n%s", body)
	}
}

func TestSearchHits_KindFilterNarrows(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	seedSession(t, st, "sess-mix", "shared marker text", now)
	// Drop a tool_use event with the same content_text so the kind
	// filter has something to narrow.
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	base, stop := startTestServer(t, st)
	defer stop()

	// kind=user_prompt should match the seeded prompt; kind=tool_use
	// should miss (no tool_use rows in this seed).
	_, body := fetch(t, base+"/search/hits?"+url.Values{
		"q":    {"shared"},
		"kind": {"user_prompt"},
	}.Encode())
	if !strings.Contains(body, "shared") {
		t.Errorf("kind=user_prompt should still find the prompt:\n%s", body)
	}

	_, missBody := fetch(t, base+"/search/hits?"+url.Values{
		"q":    {"shared"},
		"kind": {"tool_use"},
	}.Encode())
	if !strings.Contains(missBody, "(no hits for that query)") {
		t.Errorf("kind=tool_use should miss the user_prompt-only seed:\n%s", missBody)
	}
}

func TestSearchHits_SinceFilterApplied(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Now().UTC()
	// Old prompt — well outside any preset window.
	seedSession(t, st, "sess-old", "ancient marker phrase", now.Add(-90*24*time.Hour))
	// Recent prompt.
	seedSession(t, st, "sess-new", "fresh marker phrase", now.Add(-1*time.Hour))

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/search/hits?"+url.Values{
		"q":     {"marker"},
		"since": {"24h"},
	}.Encode())
	if strings.Contains(body, "ancient") {
		t.Errorf("since=24h should exclude 90-day-old row:\n%s", body)
	}
	if !strings.Contains(body, "fresh") {
		t.Errorf("since=24h should include 1-hour-old row:\n%s", body)
	}
}

func TestParseSinceWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		want   time.Duration
		wantOK bool
	}{
		{"", 0, false},
		{"24h", 24 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"30d", 30 * 24 * time.Hour, true},
		{"nonsense", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseSinceWindow(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("parseSinceWindow(%q) = (%v, %v); want (%v, %v)",
				tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestStaticAssets_HtmxAvailable(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/static/htmx.min.js")
	if status != http.StatusOK {
		t.Errorf("htmx.min.js: status %d, want 200", status)
	}
	if len(body) < 10000 {
		t.Errorf("htmx.min.js looks too small (%d bytes)", len(body))
	}
}
