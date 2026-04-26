package web

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

func TestSessionDetail_RendersHeaderAndEvents(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedSession(t, st, "sess-detail", "what does the daemon do", now)
	// Add a second event to the same session so the timeline isn't trivial.
	seedSession(t, st, "sess-detail", "follow-up question about migrations", now.Add(time.Minute))

	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/sessions/"+id)
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", status, body)
	}

	for _, want := range []string{
		id,                                    // full UUID in the header table
		"/work/sess-detail",                   // cwd
		"what does the daemon do",             // first prompt content
		"follow-up question about migrations", // second prompt content
		"user_prompt",                         // event kind badge
		"No cached summary yet",               // empty-state line for missing summary
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestSessionDetail_RendersCachedSummary(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedSession(t, st, "sess-summary", "investigate redaction", now)

	// Plant a realistic summary body matching prompts.SummaryResult shape.
	const body = `{
		"topic": "Investigate the four-layer redaction story",
		"what_was_done": [
			"Read internal/redact",
			"Confirmed daemon refuses unredacted envelopes"
		],
		"unresolved": ["Document the boundary in threat-model.md"],
		"key_files": ["internal/redact/redact.go", "internal/daemon/server.go"],
		"links": [
			{"url": "https://www.sqlite.org/fts5.html", "used_for": "verifying tokenizer options"}
		]
	}`
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   sql.NullString{String: id, Valid: true},
		Kind:        store.LLMKindSummary,
		Model:       "claude-sonnet-4-6",
		PromptHash:  "h1",
		Body:        body,
		CreatedAtMs: now.UnixMilli(),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed summary: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	baseURL, stop := startTestServer(t, st)
	defer stop()

	status, page := fetch(t, baseURL+"/sessions/"+id)
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}

	for _, want := range []string{
		"Investigate the four-layer redaction story", // topic
		"Read internal/redact",                       // what_was_done bullet
		"Document the boundary in threat-model",      // unresolved bullet
		"internal/redact/redact.go",                  // key_files
		"https://www.sqlite.org/fts5.html",           // link href
		"verifying tokenizer options",                // link used_for
		"claude-sonnet-4-6",                          // model in summary header
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q\n--- page ---\n%s", want, page)
		}
	}
}

// TestSessionDetail_IncludesLiveBannerWiring confirms the
// session-detail page subscribes to /stream filtered by its own
// session_id, so a new event for THIS session lands in the banner
// while events for other sessions don't.
func TestSessionDetail_IncludesLiveBannerWiring(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedSession(t, st, "sess-banner", "anything", now)

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/sessions/"+id)
	for _, want := range []string{
		`hx-ext="sse"`,
		`sse-connect="/stream?session_id=` + id + `"`,
		`sse-swap="event"`,
		`hx-swap="afterbegin"`,
		`id="livebanner-target"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestSessionDetail_UnknownIDIs404(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	resp, err := http.Get(base + "/sessions/00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

func TestSessionDetail_MalformedSummaryDoesNotCrash(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedSession(t, st, "sess-bad", "anything", now)

	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   sql.NullString{String: id, Valid: true},
		Kind:        store.LLMKindSummary,
		Model:       "test",
		PromptHash:  "h-bad",
		Body:        "not actually JSON {{{",
		CreatedAtMs: now.UnixMilli(),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	base, stop := startTestServer(t, st)
	defer stop()

	status, page := fetch(t, base+"/sessions/"+id)
	if status != http.StatusOK {
		t.Fatalf("malformed summary should still render the page; got %d", status)
	}
	if !strings.Contains(page, "(unparseable cached summary)") {
		t.Errorf("expected fallback marker for unparseable JSON:\n%s", page)
	}
}

func TestSessionDetail_EndedActiveLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ms   sql.NullInt64
		want string
	}{
		{"valid ended", sql.NullInt64{Int64: time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC).UnixMilli(), Valid: true},
			"2026-04-24"},
		{"null is active", sql.NullInt64{}, "(active)"},
		{"zero is active", sql.NullInt64{Int64: 0, Valid: true}, "(active)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := endedOrActive(tc.ms)
			if !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want substring %q", got, tc.want)
			}
		})
	}
}
