package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
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

func TestSearchHits_UnrecognisedSinceIsError(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/search/hits?"+url.Values{
		"q":     {"x"},
		"since": {"24hr"}, // typo: was 24h
	}.Encode())
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	if !strings.Contains(body, "unrecognised window") {
		t.Errorf("expected error fragment, got:\n%s", body)
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

func TestSearchHits_CompactCapsRowsAndAddsSeeAllLink(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	// Seed 12 prompts that all share a common token so a single
	// query returns more than the compact limit (8).
	for i := 0; i < 12; i++ {
		seedSession(t, st,
			"sess-compact-"+string(rune('a'+i)),
			"compactmarker prompt number x", // shared token "compactmarker"
			now.Add(time.Duration(i)*time.Minute))
	}

	base, stop := startTestServer(t, st)
	defer stop()

	// Compact mode caps results.
	_, compactBody := fetch(t, base+"/search/hits?"+url.Values{
		"q":       {"compactmarker"},
		"compact": {"1"},
	}.Encode())
	rowCountCompact := strings.Count(compactBody, `<a href="/sessions/`)
	if rowCountCompact != searchCompactLimit {
		t.Errorf("compact mode: got %d rows, want %d", rowCountCompact, searchCompactLimit)
	}
	// "see all" link carries the original query so /search continues
	// it on the full-page view.
	if !strings.Contains(compactBody, `href="/search?q=compactmarker"`) {
		t.Errorf("compact mode missing see-all link with original query:\n%s", compactBody)
	}

	// Default (non-compact) mode returns the full set, no see-all link.
	_, fullBody := fetch(t, base+"/search/hits?"+url.Values{
		"q": {"compactmarker"},
	}.Encode())
	rowCountFull := strings.Count(fullBody, `<a href="/sessions/`)
	if rowCountFull != 12 {
		t.Errorf("non-compact mode: got %d rows, want 12", rowCountFull)
	}
	if strings.Contains(fullBody, "see all results") {
		t.Errorf("non-compact mode should not render see-all link:\n%s", fullBody)
	}
}

func TestSearchHits_CompactDropsTableHeader(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	seedSession(t, st, "sess-hdr", "headerprobe content", now)

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/search/hits?"+url.Values{
		"q":       {"headerprobe"},
		"compact": {"1"},
	}.Encode())
	// The popover hides the table header to keep the dropdown
	// dense — verify the <thead> element is absent.
	if strings.Contains(body, "<thead>") {
		t.Errorf("compact mode should omit <thead>:\n%s", body)
	}
}

func TestNavSearch_PresentOnEveryPage(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	// The nav-bar search input rides in base.html, so every page
	// the layout wraps must include the htmx wiring. Sample a few
	// representative routes — empty store is fine, we're checking
	// markup, not data.
	for _, path := range []string{"/", "/search"} {
		_, body := fetch(t, base+path)
		for _, want := range []string{
			`class="navsearch"`,                // form wrapper
			`id="navsearch-popover"`,           // hx-target
			`hx-get="/search/hits?compact=1"`,  // compact-mode endpoint
			`hx-trigger="input changed delay:`, // type-as-you-search
			`hx-target="#navsearch-popover"`,   // popover swap target
			`action="/search"`,                 // submit falls through to full page
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %q\n--- body ---\n%s", path, want, body)
			}
		}
	}
}

func TestSearchHits_FullModeShowsSummaryTopic(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedSession(t, st, "sess-search-topic", "topicquery prompt body", now)

	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   ptrTo(id),
		Kind:        store.LLMKindSummary,
		Model:       "claude-sonnet-4-6",
		PromptHash:  "h-search-topic",
		Body:        `{"topic":"Investigate the topicquery edge case"}`,
		CreatedAtMs: now.UnixMilli(),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed summary: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	base, stop := startTestServer(t, st)
	defer stop()

	// Full search-page mode — topic line should appear under the snippet.
	_, fullBody := fetch(t, base+"/search/hits?"+url.Values{
		"q": {"topicquery"},
	}.Encode())
	if !strings.Contains(fullBody, "Investigate the topicquery edge case") {
		t.Errorf("full mode: expected topic line to render:\n%s", fullBody)
	}
	if !strings.Contains(fullBody, `<small class="topic">`) {
		t.Errorf("full mode: expected <small class=\"topic\"> wrapper:\n%s", fullBody)
	}
}

func TestSearchHits_CompactModeOmitsSummaryTopic(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedSession(t, st, "sess-popover", "popoverquery probe text", now)

	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   ptrTo(id),
		Kind:        store.LLMKindSummary,
		Model:       "claude-sonnet-4-6",
		PromptHash:  "h-popover",
		Body:        `{"topic":"Investigate the popoverquery edge case"}`,
		CreatedAtMs: now.UnixMilli(),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed summary: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	base, stop := startTestServer(t, st)
	defer stop()

	// Compact mode (popover) keeps rows dense — topic suppressed.
	_, compactBody := fetch(t, base+"/search/hits?"+url.Values{
		"q":       {"popoverquery"},
		"compact": {"1"},
	}.Encode())
	if strings.Contains(compactBody, "Investigate the popoverquery edge case") {
		t.Errorf("compact mode should not render topic:\n%s", compactBody)
	}
	if strings.Contains(compactBody, `<small class="topic">`) {
		t.Errorf("compact mode should not include topic <small> wrapper:\n%s", compactBody)
	}
}

func TestUniqueSessionIDs(t *testing.T) {
	t.Parallel()
	hits := []SearchHitRow{
		{SessionID: "a"},
		{SessionID: "b"},
		{SessionID: "a"}, // dup
		{SessionID: "c"},
		{SessionID: "b"}, // dup
	}
	got := uniqueSessionIDs(hits)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("idx %d: got %q, want %q", i, got[i], w)
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
