package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

// seedFactsLLMOutput inserts a minimal kind=facts llm_outputs row so
// semantic_facts.source_llm_output_id has an FK target.
func seedFactsLLMOutput(t *testing.T, st *store.Store) int64 {
	t.Helper()
	r, err := st.DB().Exec(
		`INSERT INTO llm_outputs(kind, model, prompt_hash, body, created_at_ms)
		 VALUES ('facts', 'fake-model', ?, '{}', ?)`,
		"h-"+t.Name(), time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("seed llm_output: %v", err)
	}
	id, _ := r.LastInsertId()
	return id
}

func TestFactsPage_IndexEmpty(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/facts")
	if status != http.StatusOK {
		t.Fatalf("status: %d; body=%s", status, body)
	}
	for _, want := range []string{
		"Facts",
		"No facts have been induced yet",
		"facts induce --session",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestFactsPage_IndexListsSubjects(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	loID := seedFactsLLMOutput(t, st)
	for _, sub := range []string{"/work/proj-a", "/work/proj-b"} {
		if _, err := store.SaveSemanticFact(context.Background(), st.DB(), store.SemanticFact{
			SourceLLMOutputID: loID,
			Subject:           sub,
			Predicate:         "primary_language",
			Object:            "Go",
			Confidence:        0.9,
			AssertedAtMs:      time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("save %s: %v", sub, err)
		}
	}

	_, body := fetch(t, base+"/facts")
	for _, want := range []string{
		"Subjects with facts",
		"/work/proj-a",
		"/work/proj-b",
		`href="/facts?subject=`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestFactsPage_DetailRendersFactsTable(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	loID := seedFactsLLMOutput(t, st)
	const sessID = "00000000-0000-0000-0000-0000000000a1"
	if _, err := st.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, 'claude-code', 'src-x')`,
		sessID,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := store.SaveSemanticFact(context.Background(), st.DB(), store.SemanticFact{
		SourceLLMOutputID: loID,
		Subject:           "/work/myproj",
		Predicate:         "uses_language_version",
		Object:            "Go 1.26",
		Confidence:        0.95,
		EvidenceSessionID: ptrTo(sessID),
		EvidenceQuote:     ptrTo("go.mod requires 1.26"),
		AssertedAtMs:      time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	_, body := fetch(t, base+"/facts?subject=%2Fwork%2Fmyproj")
	for _, want := range []string{
		"/work/myproj",
		"uses_language_version",
		"Go 1.26",
		"95%",
		"go.mod requires 1.26",
		`href="/sessions/` + sessID,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n--- body ---\n%s", want, body)
		}
	}

	// The /facts/rows htmx fragment renders the same rows via the
	// shared facts-rows partial (single subject, under the page limit
	// → no Load more row).
	status, frag := fetch(t, base+"/facts/rows?subject=%2Fwork%2Fmyproj")
	if status != http.StatusOK {
		t.Fatalf("fragment status=%d body=%s", status, frag)
	}
	if !strings.Contains(frag, "uses_language_version") {
		t.Errorf("fragment missing fact row:\n%s", frag)
	}
	if strings.Contains(frag, "Load more") {
		t.Errorf("single sub-page result should have no Load more:\n%s", frag)
	}
	// Missing subject → 400.
	if st2, _ := fetch(t, base+"/facts/rows"); st2 != http.StatusBadRequest {
		t.Errorf("/facts/rows without subject: status=%d, want 400", st2)
	}
}

func TestFactsPage_DetailEmpty(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/facts?subject=%2Fno-such-project")
	if !strings.Contains(body, "No facts known for") {
		t.Errorf("expected detail empty-state, got:\n%s", body)
	}
}
