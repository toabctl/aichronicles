package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

// seedSummaryRow plants a summary llm_outputs row for `sessionID`
// with the given topic. Used to exercise the topic-rendering path
// of the Related sessions sidebar — the linked-to session needs a
// summary or the sidebar renders just the short id.
func seedSummaryRow(t *testing.T, st *store.Store, sessionID, topic string, ts time.Time) {
	t.Helper()
	body := `{"topic":"` + topic + `","what_was_done":["x"],"unresolved":[],"key_files":[],"links":[]}`
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   ptrTo(sessionID),
		Kind:        store.LLMKindSummary,
		Model:       "fake-model",
		PromptHash:  "h-" + sessionID,
		Body:        body,
		CreatedAtMs: ts.UnixMilli(),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("save summary: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestSessionDetail_RendersRelatedSessionsSidebar(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	this := seedSession(t, st, "this-sess", "current work", now)
	priorBuiltOn := seedSession(t, st, "prior-built", "the auth fix", now.Add(-2*time.Hour))
	priorRelated := seedSession(t, st, "prior-related", "tangentially related thing", now.Add(-3*time.Hour))
	laterIncoming := seedSession(t, st, "later-incoming", "newer follow-up", now.Add(time.Hour))

	// Topics for the linked-to sessions so the sidebar shows them.
	seedSummaryRow(t, st, priorBuiltOn, "fixed the auth middleware", now.Add(-1*time.Hour))
	seedSummaryRow(t, st, laterIncoming, "newer follow-up topic", now.Add(2*time.Hour))
	// priorRelated deliberately has no summary — should render
	// without a topic.

	// Outgoing: this → priorBuiltOn (builds_on), this → priorRelated (related)
	if err := store.SaveSessionLinks(t.Context(), st.DB(), this, []store.SessionLink{
		{ToSessionID: priorBuiltOn, Kind: store.SessionLinkBuildsOn, Rationale: "extends the auth fix"},
		{ToSessionID: priorRelated, Kind: store.SessionLinkRelated, Rationale: "shared file"},
	}); err != nil {
		t.Fatalf("seed outgoing: %v", err)
	}
	// Incoming: laterIncoming → this (builds_on)
	if err := store.SaveSessionLinks(t.Context(), st.DB(), laterIncoming, []store.SessionLink{
		{ToSessionID: this, Kind: store.SessionLinkBuildsOn, Rationale: "this picked up where the current session left off"},
	}); err != nil {
		t.Fatalf("seed incoming: %v", err)
	}

	base, stop := startTestServer(t, st)
	defer stop()

	status, page := fetch(t, base+"/sessions/"+this)
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", status, page)
	}

	// Group headers + entry content.
	for _, want := range []string{
		"Related sessions",
		"Builds on",
		"Related",
		"fixed the auth middleware", // outgoing topic
		"extends the auth fix",      // outgoing rationale
		"shared file",               // outgoing rationale (related)
		"newer follow-up topic",     // incoming topic
		"this picked up where the current session left off", // incoming rationale
		"(this → other)", // direction marker for outgoing
		"(other → this)", // direction marker for incoming
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// Sanity: the priorRelated short id should appear even though
	// it has no topic (no summary).
	if !strings.Contains(page, priorRelated[:8]) {
		t.Errorf("priorRelated short id missing from page")
	}
}

func TestSessionDetail_HidesRelatedSessionsWhenNone(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedSession(t, st, "lonely", "lonely session", now)

	base, stop := startTestServer(t, st)
	defer stop()

	status, page := fetch(t, base+"/sessions/"+id)
	if status != http.StatusOK {
		t.Fatalf("status: %d", status)
	}
	if strings.Contains(page, "Related sessions") {
		t.Errorf("Related sessions sidebar should be hidden when there are no links:\n%s", page)
	}
}
