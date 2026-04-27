package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/store"
)

// addToolEvent inserts a synthetic tool_use event into an existing
// session so faceted-filter tests can assert that ?tool=Bash
// narrows the candidate set. Mirrors what the live ingest path
// would have written. Content default is "running <tool>" — pass
// addToolEventWithContent to override (search-filter tests need
// the FTS marker on the tool_use row, not just the user_prompt).
func addToolEvent(t *testing.T, st *store.Store, sessionID, toolName string, ts time.Time) {
	t.Helper()
	addToolEventWithContent(t, st, sessionID, toolName, "running "+toolName, ts)
}

func addToolEventWithContent(t *testing.T, st *store.Store, sessionID, toolName, content string, ts time.Time) {
	t.Helper()
	eventID := uuid.Must(uuid.NewV7()).String()
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES (?, (SELECT COALESCE(MAX(ingest_seq),0)+1 FROM raw_envelopes), ?, ?, ?, ?, ?)`,
		eventID, "claude-code", "src", ts.UnixMilli(), ts.UnixMilli(), "{}",
	); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, tool_name, content_text)
		 VALUES (?, ?, 'claude-code', 'tool_use', ?, ?, ?)`,
		eventID, sessionID, ts.UnixMilli(), toolName, content,
	); err != nil {
		t.Fatalf("event insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// addFailureEvent inserts a tool_failure event so the
// with-failures filter has something to find.
func addFailureEvent(t *testing.T, st *store.Store, sessionID string, ts time.Time) {
	t.Helper()
	eventID := uuid.Must(uuid.NewV7()).String()
	tx, _ := st.DB().Begin()
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES (?, (SELECT COALESCE(MAX(ingest_seq),0)+1 FROM raw_envelopes), 'claude-code', 'src', ?, ?, '{}')`,
		eventID, ts.UnixMilli(), ts.UnixMilli(),
	); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, content_text)
		 VALUES (?, ?, 'claude-code', 'tool_failure', ?, ?)`,
		eventID, sessionID, ts.UnixMilli(), "the tool failed",
	); err != nil {
		t.Fatalf("event insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// addExtraction stamps an extraction onto a session — used to seed
// skill_load and file_path facts the skill / file filters key on.
// event_id is sourced from any event already on the session so the
// FK passes; we don't need a per-extraction event for these tests.
func addExtraction(t *testing.T, st *store.Store, sessionID, kind, value string) {
	t.Helper()
	var anyEvent string
	if err := st.DB().QueryRow(
		`SELECT event_id FROM events WHERE session_id = ? LIMIT 1`, sessionID,
	).Scan(&anyEvent); err != nil {
		t.Fatalf("locate event_id for extraction seed: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO extractions(event_id, session_id, kind, value) VALUES (?, ?, ?, ?)`,
		anyEvent, sessionID, kind, value,
	); err != nil {
		t.Fatalf("extraction insert: %v", err)
	}
}

func TestSessionsPage_ToolFilterChip(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	bashID := seedSession(t, st, "sess-bash", "alpha-marker bash session", now)
	editID := seedSession(t, st, "sess-edit", "beta-marker edit session", now.Add(time.Hour))
	addToolEvent(t, st, bashID, "Bash", now.Add(time.Minute))
	addToolEvent(t, st, editID, "Edit", now.Add(time.Hour+time.Minute))

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/?tool=Bash")
	if !strings.Contains(body, "alpha-marker") {
		t.Errorf("tool=Bash should keep the bash row:\n%s", body)
	}
	if strings.Contains(body, "beta-marker") {
		t.Errorf("tool=Bash should hide non-Bash sessions:\n%s", body)
	}
	if !strings.Contains(body, "tool: Bash ✕") {
		t.Errorf("body missing tool chip:\n%s", body)
	}
	// Removing the chip should land back at /, no tool filter.
	if !strings.Contains(body, `href="/"`) {
		t.Errorf("tool chip should remove via href=\"/\":\n%s", body)
	}
}

func TestSessionsPage_SkillFilterChip(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	skID := seedSession(t, st, "sess-skill", "alpha-marker skill session", now)
	plainID := seedSession(t, st, "sess-plain", "beta-marker plain session", now.Add(time.Hour))
	_ = plainID
	addExtraction(t, st, skID, "skill_load", "test-creation")

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/?skill=test-creation")
	if !strings.Contains(body, "alpha-marker") {
		t.Errorf("skill filter should keep the loading session:\n%s", body)
	}
	if strings.Contains(body, "beta-marker") {
		t.Errorf("skill filter should hide non-loading sessions:\n%s", body)
	}
	if !strings.Contains(body, "skill: test-creation ✕") {
		t.Errorf("body missing skill chip:\n%s", body)
	}
}

func TestSessionsPage_FileFilterChip_SubstringMatch(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	fileID := seedSession(t, st, "sess-file", "alpha-marker file session", now)
	otherID := seedSession(t, st, "sess-other", "beta-marker other session", now.Add(time.Hour))
	_ = otherID
	addExtraction(t, st, fileID, "file_path", "internal/store/migrate.go")

	base, stop := startTestServer(t, st)
	defer stop()

	// Substring of the path matches.
	_, body := fetch(t, base+"/?file=migrate")
	if !strings.Contains(body, "alpha-marker") {
		t.Errorf("file substring should match the file session:\n%s", body)
	}
	if strings.Contains(body, "beta-marker") {
		t.Errorf("file substring should hide unrelated sessions:\n%s", body)
	}
	if !strings.Contains(body, "file: migrate ✕") {
		t.Errorf("body missing file chip:\n%s", body)
	}
}

func TestSessionsPage_WithFailuresChip(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	failID := seedSession(t, st, "sess-fail", "alpha-marker fail session", now)
	cleanID := seedSession(t, st, "sess-clean", "beta-marker clean session", now.Add(time.Hour))
	_ = cleanID
	addFailureEvent(t, st, failID, now.Add(time.Minute))

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/?with-failures=1")
	if !strings.Contains(body, "alpha-marker") {
		t.Errorf("with-failures should keep failing sessions:\n%s", body)
	}
	if strings.Contains(body, "beta-marker") {
		t.Errorf("with-failures should hide clean sessions:\n%s", body)
	}
	if !strings.Contains(body, "with failures ✕") {
		t.Errorf("body missing with-failures chip:\n%s", body)
	}
}

func TestSessionsPage_MultipleFiltersCombine(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	// Three sessions:
	//  - matchID: claude-code AND has Bash → wins
	//  - bashID:  gemini-cli  AND has Bash → fails agent filter
	//  - ccID:    claude-code AND no Bash  → fails tool filter
	matchID := seedSessionFull(t, st, "claude-code", "sess-match", "alpha-marker match", "/work/m", now)
	bashID := seedSessionFull(t, st, "gemini-cli", "sess-bash", "beta-marker bash", "/work/b", now.Add(time.Hour))
	_ = seedSessionFull(t, st, "claude-code", "sess-cc", "gamma-marker cc", "/work/c", now.Add(2*time.Hour))
	addToolEvent(t, st, matchID, "Bash", now.Add(time.Minute))
	addToolEvent(t, st, bashID, "Bash", now.Add(time.Hour+time.Minute))

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/?agent=claude-code&tool=Bash")
	if !strings.Contains(body, "alpha-marker") {
		t.Errorf("AND-combine should keep the matching row:\n%s", body)
	}
	for _, gone := range []string{"beta-marker", "gamma-marker"} {
		if strings.Contains(body, gone) {
			t.Errorf("AND-combine should hide %s:\n%s", gone, body)
		}
	}
	// Both chips render; removing one preserves the other.
	if !strings.Contains(body, "agent: claude-code ✕") {
		t.Errorf("agent chip missing:\n%s", body)
	}
	if !strings.Contains(body, "tool: Bash ✕") {
		t.Errorf("tool chip missing:\n%s", body)
	}
	if !strings.Contains(body, `href="/?tool=Bash"`) {
		t.Errorf("removing agent should leave tool=Bash in URL:\n%s", body)
	}
	if !strings.Contains(body, `href="/?agent=claude-code"`) {
		t.Errorf("removing tool should leave agent=claude-code in URL:\n%s", body)
	}
}

func TestSessionsPage_ClearAllChip(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedSession(t, st, "sess-only", "alpha-marker", now)
	addToolEvent(t, st, id, "Bash", now.Add(time.Minute))

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/?tool=Bash&with-failures=1")
	// "clear all" link present any time at least one filter is on.
	if !strings.Contains(body, `<a href="/" class="agent-chip" title="Clear all filters">clear all</a>`) {
		t.Errorf("clear-all chip missing:\n%s", body)
	}
}

// TestSearchPage_FacetedChipsRender confirms /search surfaces the
// same chip UI and seeds hidden form fields so the htmx fragment
// fetches /search/hits with the facet narrowing applied.
func TestSearchPage_FacetedChipsRender(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/search?agent=claude-code&tool=Bash&with-failures=1")
	for _, want := range []string{
		"agent: claude-code ✕",
		"tool: Bash ✕",
		"with failures ✕",
		`<input type="hidden" name="agent" value="claude-code">`,
		`<input type="hidden" name="tool" value="Bash">`,
		`<input type="hidden" name="with-failures" value="1">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/search missing %q\n%s", want, body)
		}
	}
}

// TestSearchHits_FacetsNarrowResults verifies the htmx fragment
// /search/hits actually applies the facet filters end-to-end.
func TestSearchHits_FacetsNarrowResults(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	// "alphamarker" is a single FTS5 token (no hyphen) so the
	// porter / unicode61 tokenizers index it intact. Hyphenated
	// markers tokenize into separate words, which would still
	// match a search for either half but is more brittle for an
	// exact-string assertion in the body.
	keepID := seedSessionFull(t, st, "claude-code", "sess-keep", "alphamarker keepme", "/work/k", now)
	dropID := seedSessionFull(t, st, "gemini-cli", "sess-drop", "alphamarker dropme", "/work/d", now.Add(time.Hour))
	// Tool events carry the marker too — facet filtering on
	// search hits is event-level (matches CLI #96 semantics), so
	// the FTS-matched event must itself satisfy the facet.
	addToolEventWithContent(t, st, keepID, "Bash", "alphamarker bash on keepme", now.Add(time.Minute))
	addToolEventWithContent(t, st, dropID, "Edit", "alphamarker edit on dropme", now.Add(time.Hour+time.Minute))

	base, stop := startTestServer(t, st)
	defer stop()

	// Without facets: both rows hit on the shared marker.
	_, all := fetch(t, base+"/search/hits?q=alphamarker")
	if !strings.Contains(all, "keepme") || !strings.Contains(all, "dropme") {
		t.Errorf("unfiltered hits should include both:\n%s", all)
	}

	// With agent+tool: only keepme survives.
	_, narrow := fetch(t, base+"/search/hits?q=alphamarker&agent=claude-code&tool=Bash")
	if !strings.Contains(narrow, "keepme") {
		t.Errorf("agent+tool hits should keep keepme:\n%s", narrow)
	}
	if strings.Contains(narrow, "dropme") {
		t.Errorf("agent+tool hits should hide dropme:\n%s", narrow)
	}
}

func TestParseTruthy(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"no":    false,
		"off":   false,
		"1":     true,
		"true":  true,
		"yes":   true,
		"on":    true,
		"YES":   true,
		"True":  true,
		" 1 ":   true,
	}
	for in, want := range cases {
		if got := parseTruthy(in); got != want {
			t.Errorf("parseTruthy(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFiltersToURL_OmitsEmpties(t *testing.T) {
	t.Parallel()
	cases := []struct {
		f    sessionListFilters
		want string
	}{
		{sessionListFilters{}, "/"},
		{sessionListFilters{Agent: "claude-code"}, "/?agent=claude-code"},
		{
			sessionListFilters{Tool: "Bash", WithFailures: true},
			"/?tool=Bash&with-failures=1",
		},
		// url.Values.Encode sorts keys alphabetically.
		{
			sessionListFilters{Agent: "x", File: "migrate.go", Skill: "y"},
			"/?agent=x&file=migrate.go&skill=y",
		},
	}
	for _, tc := range cases {
		if got := filtersToURL("/", tc.f); got != tc.want {
			t.Errorf("filtersToURL(%+v) = %q, want %q", tc.f, got, tc.want)
		}
	}
}

func TestBuildSessionListChips_OrderAndRemoval(t *testing.T) {
	t.Parallel()
	chips := buildSessionListChips("/", sessionListFilters{
		Agent:        "claude-code",
		Project:      "/work/proj",
		Tool:         "Bash",
		Skill:        "test-creation",
		File:         "migrate.go",
		WithFailures: true,
	})
	wantLabels := []string{
		"project: /work/proj",
		"agent: claude-code",
		"tool: Bash",
		"skill: test-creation",
		"file: migrate.go",
		"with failures",
	}
	if len(chips) != len(wantLabels) {
		t.Fatalf("chip count: got %d, want %d", len(chips), len(wantLabels))
	}
	for i, want := range wantLabels {
		if chips[i].Label != want {
			t.Errorf("chip[%d] label = %q, want %q", i, chips[i].Label, want)
		}
	}
	// Each removal URL must drop ITS OWN key but keep the others.
	for _, chip := range chips {
		if strings.Contains(chip.HrefRemove, "TODO_NEVER_HAPPENS") {
			t.Errorf("removal URL malformed: %q", chip.HrefRemove)
		}
	}
	// Spot-check one: removing "tool: Bash" keeps everything else.
	for _, chip := range chips {
		if chip.Label == "tool: Bash" {
			if strings.Contains(chip.HrefRemove, "tool=Bash") {
				t.Errorf("tool removal still has tool=Bash: %q", chip.HrefRemove)
			}
			if !strings.Contains(chip.HrefRemove, "agent=claude-code") {
				t.Errorf("tool removal lost agent: %q", chip.HrefRemove)
			}
			if !strings.Contains(chip.HrefRemove, "with-failures=1") {
				t.Errorf("tool removal lost with-failures: %q", chip.HrefRemove)
			}
		}
	}
}

func TestSessionsHandler_BadFilterStillReturns200(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()
	// Empty store; nonsense filters; should render an empty page,
	// not 500.
	status, _ := fetch(t, base+"/?tool=Nonexistent&skill=ghost&with-failures=1")
	if status != http.StatusOK {
		t.Errorf("expected 200 for empty-result filtered page, got %d", status)
	}
}
