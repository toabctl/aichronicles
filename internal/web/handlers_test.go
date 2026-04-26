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

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
)

// seedSession ingests one user_prompt envelope into the store and
// returns the derived session id. Used to give the sessions
// handler something to render. ts is the source timestamp; vary
// it per call to test recency ordering.
func seedSession(t *testing.T, st *store.Store, sourceSession, prompt string, ts time.Time) string {
	t.Helper()
	env := &ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sourceSession,
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        ts.UTC(),
		Cwd:             "/work/" + sourceSession,
		ContentText:     prompt,
		Payload:         map[string]any{},
		Transport:       "hook",
		Redaction:       &ingest.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(env)
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.IngestEnvelope(t.Context(), tx, env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("IngestEnvelope: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return ingest.DeriveSessionID("claude-code", sourceSession)
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
		SessionID:   sql.NullString{String: withSummary, Valid: true},
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
	// session with the summary), not twice.
	const badge = `class="badge badge-summary">summary`
	count := strings.Count(body, badge)
	if count != 1 {
		t.Errorf("badge count: got %d, want 1\n%s", count, body)
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
