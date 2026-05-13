package web

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
)

// seedSession ingests one user_prompt envelope into the store and
// returns the derived session id. Used to give the sessions
// handler something to render. ts is the source timestamp; vary
// it per call to test recency ordering.
func seedSession(t *testing.T, st *store.Store, sourceSession, prompt string, ts time.Time) string {
	t.Helper()
	return seedSessionWithCwd(t, st, sourceSession, prompt, "/work/"+sourceSession, ts)
}

// seedSessionWithCwd is seedSession with an explicit cwd, used by
// tests that exercise cwd-changes-mid-session behaviour (start vs.
// latest cwd selection for the resume button).
func seedSessionWithCwd(t *testing.T, st *store.Store, sourceSession, prompt, cwd string, ts time.Time) string {
	t.Helper()
	return seedSessionFull(t, st, "claude-code", sourceSession, prompt, cwd, ts)
}

// seedSessionFull is the most-flexible seeder, accepting an
// explicit source_agent. Used by source-agent filter tests that
// need to mix claude-code and gemini-cli sessions in one store.
func seedSessionFull(t *testing.T, st *store.Store, sourceAgent, sourceSession, prompt, cwd string, ts time.Time) string {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     sourceAgent,
		SourceSessionID: sourceSession,
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        ts.UTC(),
		Cwd:             cwd,
		ContentText:     prompt,
		Payload:         map[string]any{},
		Transport:       "hook",
		Redaction:       &events.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(env)
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.IngestEnvelope(t.Context(), tx, env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("IngestEnvelope: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return events.DeriveSessionID(sourceAgent, sourceSession)
}

// fetch is a one-liner that fetches and reads-fully a URL. Tests
// that need both status and body get them without scattering body
// closes around.
func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestSessionsPage_RendersAllSessions(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	seedSession(t, st, "sess-foo", "how do I parse jsonl in Go", now)
	seedSession(t, st, "sess-bar", "explain systemd socket activation", now.Add(time.Hour))

	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", status, body)
	}
	for _, want := range []string{
		"Sessions",               // page heading
		"how do I parse jsonl",   // foo prompt preview
		"explain systemd socket", // bar prompt preview
		"/work/sess-foo", "/work/sess-bar",
		`<a href="/sessions/`, // each row links to detail page
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestSessionsPage_AgentFilterChips covers the source-agent
// filter UI: chips render for every distinct agent in the store,
// the active chip is marked, ?agent=<slug> narrows the list, and
// rows for the other agent disappear.
func TestSessionsPage_AgentFilterChips(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	seedSessionFull(t, st, "claude-code", "sess-cc", "claude prompt alpha-marker",
		"/work/cc", now)
	seedSessionFull(t, st, "gemini-cli", "sess-gem", "gemini prompt beta-marker",
		"/work/gem", now.Add(time.Hour))

	base, stop := startTestServer(t, st)
	defer stop()

	// No filter: both rows present + both chips rendered.
	_, body := fetch(t, base+"/")
	for _, want := range []string{
		`class="agent-filter"`,
		`href="/?agent=claude-code"`,
		`href="/?agent=gemini-cli"`,
		"alpha-marker",
		"beta-marker",
		`class="agent-chip agent-chip-active"`, // "all" chip is active
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unfiltered page missing %q\n%s", want, body)
		}
	}

	// Filter by gemini-cli: only the gemini row appears, claude row is gone.
	_, gemBody := fetch(t, base+"/?agent=gemini-cli")
	if !strings.Contains(gemBody, "beta-marker") {
		t.Errorf("filtered page missing gemini row:\n%s", gemBody)
	}
	if strings.Contains(gemBody, "alpha-marker") {
		t.Errorf("filtered page should hide claude-code row:\n%s", gemBody)
	}
	// The gemini chip should be the active one.
	if !strings.Contains(gemBody, `href="/?agent=gemini-cli" class="agent-chip agent-chip-active"`) {
		t.Errorf("gemini chip should be active:\n%s", gemBody)
	}
}

func TestSessionsPage_IncludesLiveFeedWiring(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/")
	for _, want := range []string{
		`hx-ext="sse"`,
		`sse-connect="/stream"`,
		`sse-swap="event"`,
		`hx-swap="afterbegin"`,
		`id="livefeed"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestSessionsPage_EmptyStoreShowsEmptyMessage(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	if !strings.Contains(body, "no sessions in the store yet") {
		t.Errorf("expected empty-state line:\n%s", body)
	}
}

func TestSessionsPage_OrdersNewestFirst(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	seedSession(t, st, "sess-old", "old work alpha-marker", older)
	seedSession(t, st, "sess-new", "new work beta-marker", newer)

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/")
	betaIdx := strings.Index(body, "beta-marker")
	alphaIdx := strings.Index(body, "alpha-marker")
	if betaIdx < 0 || alphaIdx < 0 {
		t.Fatalf("both prompts should render; got beta=%d alpha=%d\n%s", betaIdx, alphaIdx, body)
	}
	if betaIdx >= alphaIdx {
		t.Errorf("newer (beta) should appear before older (alpha): beta=%d alpha=%d", betaIdx, alphaIdx)
	}
}

func TestSessionsPage_HasSummaryBadgeOnlyForCachedRows(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	withSummary := seedSession(t, st, "sess-with", "summarised work", now)
	seedSession(t, st, "sess-without", "unsummarised work", now.Add(time.Minute))

	// Plant an llm_outputs row for one session.
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   ptrTo(withSummary),
		Kind:        store.LLMKindSummary,
		Model:       "test",
		PromptHash:  "h-summary",
		Body:        `{"topic":"x"}`,
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

	_, body := fetch(t, base+"/")
	// Crude but sufficient: the badge HTML appears once (for the
	// session with the summary), not twice. Anchored on the
	// classnames-up-to-the-quote so the assertion is stable across
	// later attribute additions (e.g. title= for the hover tooltip).
	const badgeMarker = `class="badge badge-summary"`
	count := strings.Count(body, badgeMarker)
	if count != 1 {
		t.Errorf("badge count: got %d, want 1\n%s", count, body)
	}
}

func TestSessionsPage_ShowsSummaryTopicInRow(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	withTopic := seedSession(t, st, "sess-topical", "long question text", now)
	seedSession(t, st, "sess-plain", "another question", now.Add(time.Minute))

	// Plant a parseable summary on the first session only.
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   ptrTo(withTopic),
		Kind:        store.LLMKindSummary,
		Model:       "claude-sonnet-4-6",
		PromptHash:  "h-topic",
		Body:        `{"topic":"Reproducing the kitten kerning bug"}`,
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

	_, body := fetch(t, base+"/")
	// Summary topic now drives the row's primary preview cell —
	// the model's distillation is a far better signal than the
	// first user_prompt, which is often filler ("yes", "/loop").
	if !strings.Contains(body, `class="preview preview-topic">Reproducing the kitten kerning bug</span>`) {
		t.Errorf("topic should render as the primary preview for summarised sessions:\n%s", body)
	}
	// And the un-summarised sister session should NOT show its
	// short first_prompt — "another question" is below the
	// substantiveMinRunes floor, so the row falls through to the
	// muted "(no summary yet)" placeholder.
	if !strings.Contains(body, `class="preview preview-muted">(no summary yet)</span>`) {
		t.Errorf("un-summarised + short first_prompt should render the muted placeholder:\n%s", body)
	}
}

func TestSessionsPage_SummaryBadgeCarriesTooltip(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	id := seedSession(t, st, "sess-tooltip", "anything", now)

	// Plant a summary with topic + what_was_done + unresolved so
	// every section of the tooltip renderer is exercised.
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:  ptrTo(id),
		Kind:       store.LLMKindSummary,
		Model:      "claude-sonnet-4-6",
		PromptHash: "h-tooltip",
		Body: `{
			"topic": "Investigate the foo bar baz drift",
			"what_was_done": ["Read internal/foo", "Wrote a regression test", "Filed PR #42"],
			"unresolved": ["Document the new invariant"],
			"key_files": ["a.go"],
			"links": []
		}`,
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

	_, body := fetch(t, base+"/")
	// The badge MUST carry a title= attribute. The tooltip text is
	// auto-escaped by html/template, so newlines become "&#10;" or
	// similar — assert on the topic and the bullet labels rather
	// than literal newlines.
	if !strings.Contains(body, `class="badge badge-summary" title=`) {
		t.Errorf("expected summary badge with title= attribute:\n%s", body)
	}
	for _, want := range []string{
		"Investigate the foo bar baz drift", // topic line
		"What was done:",                    // header
		"Read internal/foo",                 // first what_was_done bullet
		"Filed PR #42",                      // last what_was_done bullet
		"Unresolved:",                       // unresolved header
		"Document the new invariant",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tooltip body missing %q:\n%s", want, body)
		}
	}
}

func TestSessionsPage_MalformedSummaryBodyOmitsTopic(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedSession(t, st, "sess-malformed", "another question", now)

	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   ptrTo(id),
		Kind:        store.LLMKindSummary,
		Model:       "test",
		PromptHash:  "h-bad-list",
		Body:        "not actually JSON",
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

	status, body := fetch(t, base+"/")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	// Badge still appears (the row has a summary), but the topic
	// line must be absent because the JSON didn't parse.
	if !strings.Contains(body, "badge-summary") {
		t.Errorf("expected summary badge even for malformed body:\n%s", body)
	}
	if strings.Contains(body, `<small class="topic">`) {
		t.Errorf("malformed summary must not render an empty topic line:\n%s", body)
	}
}

func TestSessionsPage_StatusDotAndLatestColumnPresent(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	id := seedSession(t, st, "sess-row", "anything", time.Now().Add(-1*time.Minute))

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/")
	for _, want := range []string{
		// Status dot wiring per row — id must be "status-<sessionID>"
		// so the SSE OOB swap can target it.
		`id="status-` + id + `"`,
		`class="status status-`,
		// Latest-event cell must declare its SSE event name + swap
		// strategy so live updates can replace its content.
		`sse-swap="session-` + id + `"`,
		`hx-swap="innerHTML"`,
		// Single shared SSE container wraps the live feed AND the
		// table — one connection drives all updates.
		`hx-ext="sse"`,
		`sse-connect="/stream"`,
		// The new column header.
		"<th>Latest event</th>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestSessionsPage_StatusActiveForRecentSession(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	// Seed an event within the activity window so the row is active.
	seedSession(t, st, "sess-active", "fresh prompt", time.Now().Add(-30*time.Second))

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/")
	if !strings.Contains(body, "status-active") {
		t.Errorf("expected status-active class for recent session:\n%s", body)
	}
}

func TestSessionsPage_StatusIdleForStaleSession(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	// Seed an event well outside the 5-min window. now := time.Now()
	// is a moving target, so use a fixed-ish offset.
	seedSession(t, st, "sess-stale", "old prompt", time.Now().Add(-1*time.Hour))

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/")
	if !strings.Contains(body, "status-idle") {
		t.Errorf("expected status-idle class for stale session:\n%s", body)
	}
}

func TestSessionsPage_StatusEndedWhenSessionEndEventPresent(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	// Seed a normal user_prompt first, then a session_end event. The
	// session_end event becomes the "latest event" and flips the
	// status dot — same path the SessionEnd hook fires on real claude
	// sessions.
	seedSession(t, st, "sess-ended", "wrapping up", time.Now().Add(-1*time.Minute))
	seedEnv := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-ended",
		Kind:            "session_end",
		Role:            "system",
		TsSource:        time.Now().UTC(),
		Cwd:             "/work/sess-ended",
		Payload:         map[string]any{"reason": "user-quit"},
		Transport:       "hook",
		Redaction:       &events.Redaction{Applied: true},
	}
	rawBytes, _ := json.Marshal(seedEnv)
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.IngestEnvelope(t.Context(), tx, seedEnv, rawBytes, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ingest session_end: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/")
	if !strings.Contains(body, "status-ended") {
		t.Errorf("expected status-ended class for ended session:\n%s", body)
	}
	if strings.Contains(body, "status-active") {
		t.Errorf("ended session should not also render status-active:\n%s", body)
	}
}

func TestSessionsPage_LatestEventCellRendersInitialContent(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	seedSession(t, st, "sess-latest", "latestmarkerprompt content", time.Now().Add(-10*time.Second))

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/")
	// The latest-event cell should contain the kind badge and the
	// truncated snippet — same renderer the SSE handler will use.
	if !strings.Contains(body, "latestmarkerprompt") {
		t.Errorf("latest cell should include the event snippet:\n%s", body)
	}
	if !strings.Contains(body, `<span class="badge badge-user_prompt">user_prompt</span>`) {
		t.Errorf("latest cell should include the kind-coloured badge:\n%s", body)
	}
}

func TestSessionsPage_LatestCellEmptyForSessionWithNoEvents(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)

	// Insert a session row directly without going through ingest, so
	// it has zero events. This is how an empty session would show up
	// if (somehow) the row existed before any event landed.
	if _, err := st.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, event_count)
		 VALUES (?, ?, ?, ?, ?)`,
		"empty-session-id", "claude-code", "empty", time.Now().UnixMilli(), 0,
	); err != nil {
		t.Fatalf("seed empty session: %v", err)
	}

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/")
	// Empty-state placeholder lives inside the latest cell so the
	// table has no awkward blank cells.
	if !strings.Contains(body, `<span class="empty">—</span>`) {
		t.Errorf("expected em-dash placeholder for session with no events:\n%s", body)
	}
}

func TestStaticAssets_Served(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	for _, path := range []string{"/static/pico.min.css", "/static/app.css"} {
		status, body := fetch(t, base+path)
		if status != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", path, status)
		}
		if len(body) == 0 {
			t.Errorf("GET %s: empty body", path)
		}
	}
}

func TestRelativeTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		ms   int64
		want string
	}{
		{"zero ts", 0, "-"},
		{"future ts", now.Add(time.Hour).UnixMilli(), "-"},
		{"sub-minute", now.Add(-30 * time.Second).UnixMilli(), "just now"},
		{"30 minutes", now.Add(-30 * time.Minute).UnixMilli(), "30m ago"},
		{"5 hours", now.Add(-5 * time.Hour).UnixMilli(), "5h ago"},
		{"3 days", now.Add(-3 * 24 * time.Hour).UnixMilli(), "3d ago"},
		{"older than 30 days uses absolute date", time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), "2025-12-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := relativeTime(tc.ms, now); got != tc.want {
				t.Errorf("relativeTime: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTruncatePreview(t *testing.T) {
	t.Parallel()
	// Whitespace flattening + rune cap behaviour.
	cases := []struct {
		in   sql.NullString
		want string
	}{
		{sql.NullString{}, "-"},
		{sql.NullString{Valid: true, String: ""}, "-"},
		{sql.NullString{Valid: true, String: "hello\nworld"}, "hello world"},
		{sql.NullString{Valid: true, String: "  short  "}, "  short  "},
	}
	for _, tc := range cases {
		if got := truncatePreview(tc.in); got != tc.want {
			t.Errorf("truncatePreview(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
