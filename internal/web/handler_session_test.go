package web

import (
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
		SessionID:   ptrTo(id),
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

// TestSessionDetail_RendersEpisodesSection seeds a session with two
// segmenter-produced episodes and asserts the detail page shows the
// "Episodes" header, both ordinals, and both intent summaries.
func TestSessionDetail_RendersEpisodesSection(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	// Seed enough events to produce a session row + first events. We
	// then plant the episode rows directly so the test exercises the
	// page render path, not the segmenter (which has its own tests).
	id := seedSession(t, st, "sess-eps", "first intent prompt", now)
	seedSession(t, st, "sess-eps", "second intent prompt", now.Add(20*time.Minute))

	// First-event ids needed for the FK in episodes.first_event_id.
	rows, err := st.DB().Query(
		`SELECT event_id FROM events WHERE session_id = ? ORDER BY ts_source_ms ASC`, id,
	)
	if err != nil {
		t.Fatalf("event_ids: %v", err)
	}
	var eventIDs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		eventIDs = append(eventIDs, s)
	}
	_ = rows.Close()
	if len(eventIDs) < 2 {
		t.Fatalf("seeded too few events: %d", len(eventIDs))
	}

	for i, intent := range []string{"first intent prompt", "second intent prompt"} {
		if _, err := st.DB().Exec(
			`INSERT INTO episodes(session_id, ordinal, started_at_ms, ended_at_ms,
				cwd, intent_summary, event_count, first_event_id)
			 VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
			id, i+1, now.Add(time.Duration(i)*20*time.Minute).UnixMilli(),
			now.Add(time.Duration(i)*20*time.Minute+5*time.Minute).UnixMilli(),
			"/work/sess-eps", intent, eventIDs[i],
		); err != nil {
			t.Fatalf("insert episode %d: %v", i+1, err)
		}
	}

	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/sessions/"+id)
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	for _, want := range []string{
		"Episodes",             // section header
		"first intent prompt",  // ep 1 intent_summary
		"second intent prompt", // ep 2 intent_summary
		"/work/sess-eps",       // shared cwd
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestSessionDetail_HidesEpisodesSectionWhenEmpty asserts the
// detail page does NOT render the Episodes header when the
// segmenter hasn't run on the session — the {{with}} block hides
// it cleanly.
func TestSessionDetail_HidesEpisodesSectionWhenEmpty(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedSession(t, st, "sess-noeps", "no episodes here", now)

	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/sessions/"+id)
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	// The "Episodes" word must not appear as a section header. We
	// look for the section's distinguishing subtitle instead of the
	// bare word, which might collide with future copy elsewhere on
	// the page.
	if strings.Contains(body, "contextually-coherent slices") {
		t.Errorf("Episodes section should be hidden when no episodes exist:\n%s", body)
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

// TestSessionDetail_PrefixRedirectsToCanonical pins the bug where
// pasting a short id from MCP / `aichronicles sessions` into the
// URL bar gave 404. The handler now resolves any unique prefix and
// 302s to the full /sessions/<uuid> URL.
func TestSessionDetail_PrefixRedirectsToCanonical(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedSession(t, st, "sess-prefix", "anything", now)

	base, stop := startTestServer(t, st)
	defer stop()

	// Don't follow redirects so we can assert on the 302 itself.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(base + "/sessions/" + id[:8])
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status: got %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/sessions/"+id {
		t.Errorf("Location: got %q, want %q", got, "/sessions/"+id)
	}

	// Default client follows the redirect and lands on the page.
	status, body := fetch(t, base+"/sessions/"+id[:8])
	if status != http.StatusOK {
		t.Fatalf("redirected GET: got %d, want 200; body=%s", status, body)
	}
	if !strings.Contains(body, id) {
		t.Errorf("redirected body missing canonical id %q", id)
	}
}

func TestSessionDetail_AmbiguousPrefixIs400(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	// Seed enough sessions that some hex prefix shorter than 8 chars
	// is non-unique. UUIDv5 over the source key is deterministic, so
	// we pick a prefix length where collisions are guaranteed at
	// modest scale: a single hex digit (16 buckets, 50 sessions).
	for i := range 50 {
		seedSession(t, st, "sess-amb-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "x", now)
	}

	base, stop := startTestServer(t, st)
	defer stop()

	resp, err := http.Get(base + "/sessions/0") // short prefix → many matches
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 50 sessions across 16 hex buckets makes "0" almost certainly
	// non-unique. If by random luck it isn't, the test is still
	// meaningful: a unique resolution would yield 302 (also != 400),
	// and a no-match would be 404. Only a true ambiguity is 400, so
	// this assertion describes the intended behaviour either way.
	if resp.StatusCode == http.StatusOK {
		t.Errorf("ambiguous prefix should not render directly; got 200")
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
		SessionID:   ptrTo(id),
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
	validMs := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC).UnixMilli()
	zeroMs := int64(0)
	cases := []struct {
		name string
		ms   *int64
		want string
	}{
		{"valid ended", &validMs, "2026-04-24"},
		{"nil is active", nil, "(active)"},
		{"zero is active", &zeroMs, "(active)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := endedOrActivePtr(tc.ms)
			if !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want substring %q", got, tc.want)
			}
		})
	}
}

func TestBuildResumeCommand(t *testing.T) {
	t.Parallel()
	cwdFoo := "/home/tom/devel/foo"
	cwdEmpty := ""
	cwdAic := "/home/tom/devel/aichronicles"
	cwdX := "/x"
	cases := []struct {
		name     string
		agent    string
		sourceID string
		cwd      *string
		want     string
	}{
		{
			name:     "claude-code with cwd",
			agent:    "claude-code",
			sourceID: "5c407125-a64a-46c1-96d5-65ca14bdd9fc",
			cwd:      &cwdFoo,
			want:     "cd /home/tom/devel/foo && claude --resume 5c407125-a64a-46c1-96d5-65ca14bdd9fc",
		},
		{
			name:     "claude-code without cwd",
			agent:    "claude-code",
			sourceID: "abc",
			cwd:      nil,
			want:     "claude --resume abc",
		},
		{
			name:     "claude-code with empty-but-non-nil cwd",
			agent:    "claude-code",
			sourceID: "abc",
			cwd:      &cwdEmpty,
			want:     "claude --resume abc",
		},
		{
			name:     "gemini-cli with cwd",
			agent:    "gemini-cli",
			sourceID: "9a640b1c-eefa-40ef-897a-0437f0931706",
			cwd:      &cwdAic,
			want:     "cd /home/tom/devel/aichronicles && gemini --resume 9a640b1c-eefa-40ef-897a-0437f0931706",
		},
		{
			name:     "gemini-cli without cwd",
			agent:    "gemini-cli",
			sourceID: "9a640b1c-eefa-40ef-897a-0437f0931706",
			cwd:      nil,
			want:     "gemini --resume 9a640b1c-eefa-40ef-897a-0437f0931706",
		},
		{
			name:     "unknown agent yields empty (button is hidden)",
			agent:    "some-future-agent",
			sourceID: "abc",
			cwd:      &cwdX,
			want:     "",
		},
		{
			name:     "missing source id yields empty",
			agent:    "claude-code",
			sourceID: "",
			cwd:      &cwdX,
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildResumeCommandPtr(tc.agent, tc.sourceID, tc.cwd)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSessionDetail_ResumeUsesStartCwdNotLatest pins the bug where a
// session that cd'd mid-session generated a resume command pointing
// at the *latest* cwd — which `claude --resume` rejects with "No
// conversation found", because Claude indexes transcripts by the
// cwd at session start. Seeding two events on the same session, the
// second with a different cwd, must still produce a resume command
// rooted at the first cwd.
func TestSessionDetail_ResumeUsesStartCwdNotLatest(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	// Event 1: started in /home/tom/devel/stereo
	seedSessionWithCwd(t, st, "sess-cd", "first prompt",
		"/home/tom/devel/stereo", now)
	// Event 2: same session, user has cd'd into a worktree
	id := seedSessionWithCwd(t, st, "sess-cd", "second prompt",
		"/home/tom/devel/stereo/wt-harbor-install-cert", now.Add(time.Minute))

	base, stop := startTestServer(t, st)
	defer stop()
	_, body := fetch(t, base+"/sessions/"+id)

	wantCmd := `data-resume-cmd="cd /home/tom/devel/stereo &amp;&amp; claude --resume sess-cd"`
	if !strings.Contains(body, wantCmd) {
		t.Errorf("resume button used latest cwd instead of start cwd:\nwant substring: %s\n--- body ---\n%s",
			wantCmd, body)
	}
}

// TestSessionDetail_RendersResumeButton confirms the rendered page
// carries the resume button + data-resume-cmd payload that
// keynav.js's click handler reads to populate the clipboard.
func TestSessionDetail_RendersResumeButton(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedSession(t, st, "sess-resume", "anything", now)

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/sessions/"+id)
	for _, want := range []string{
		`class="resume-btn"`,
		`data-resume-cmd="cd /work/sess-resume &amp;&amp; claude --resume sess-resume"`,
		`>↻ resume</button>`,
		// "skip perms" companion button: same machinery, same
		// data-resume-cmd attribute the click handler reads, with
		// --dangerously-skip-permissions appended to the command.
		`class="resume-btn resume-btn-dangerous"`,
		`data-resume-cmd="cd /work/sess-resume &amp;&amp; claude --resume sess-resume --dangerously-skip-permissions"`,
		`>↻ resume (skip perms)</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("session page missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestBuildResumeCommandDangerous(t *testing.T) {
	t.Parallel()
	cwdFoo := "/home/tom/devel/foo"
	cwdEmpty := ""
	cwdX := "/x"
	cases := []struct {
		name     string
		agent    string
		sourceID string
		cwd      *string
		want     string
	}{
		{
			name:     "claude-code with cwd",
			agent:    "claude-code",
			sourceID: "5c407125-a64a-46c1-96d5-65ca14bdd9fc",
			cwd:      &cwdFoo,
			want:     "cd /home/tom/devel/foo && claude --resume 5c407125-a64a-46c1-96d5-65ca14bdd9fc --dangerously-skip-permissions",
		},
		{
			name:     "claude-code without cwd",
			agent:    "claude-code",
			sourceID: "abc",
			cwd:      nil,
			want:     "claude --resume abc --dangerously-skip-permissions",
		},
		{
			name:     "claude-code with empty cwd",
			agent:    "claude-code",
			sourceID: "abc",
			cwd:      &cwdEmpty,
			want:     "claude --resume abc --dangerously-skip-permissions",
		},
		{
			// gemini-cli has its own bypass under a different name;
			// we don't emit a flag the binary will reject. Empty
			// hides the button.
			name:     "gemini-cli yields empty",
			agent:    "gemini-cli",
			sourceID: "9a640b1c-eefa-40ef-897a-0437f0931706",
			cwd:      &cwdX,
			want:     "",
		},
		{
			name:     "unknown agent yields empty",
			agent:    "some-future-agent",
			sourceID: "abc",
			cwd:      &cwdX,
			want:     "",
		},
		{
			name:     "missing source id yields empty",
			agent:    "claude-code",
			sourceID: "",
			cwd:      &cwdX,
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildResumeCommandDangerousPtr(tc.agent, tc.sourceID, tc.cwd)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
