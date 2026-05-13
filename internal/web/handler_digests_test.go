package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/store"
)

// seedWeeklyDigest writes a synthetic reflect_weekly row to
// llm_outputs in the exact shape `aichronicles digest weekly`
// produces: a bare prompts.ReflectionResult JSON in body
// (envelope wrapping is computed at render time, not stored —
// see internal/cli/digest.go). promptHashSalt disambiguates rows
// for SaveLLMOutput's (kind, prompt_hash) dedup. Returns the row
// id so caller assertions can target "id N" links / fragments.
func seedWeeklyDigest(t *testing.T, st *store.Store, result prompts.ReflectionResult, promptHashSalt string, createdAtMs int64) int64 {
	t.Helper()
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		// Weekly digests are multi-session — session_id is NULL.
		SessionID:   nil,
		Kind:        store.LLMKindReflectWeekly,
		Model:       "claude-sonnet-4-6",
		PromptHash:  "h-" + promptHashSalt,
		Body:        string(body),
		CreatedAtMs: createdAtMs,
	})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("save digest: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

func TestDigestsPage_RendersHeaderAndEmptyState(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/digests")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	for _, want := range []string{
		"Weekly digests",
		"reflect_weekly",
		"No weekly digests yet",
		"aichronicles digest weekly",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/digests missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestDigestsPage_RendersStoredCard(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)

	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	id := seedWeeklyDigest(t, st, prompts.ReflectionResult{
		TaskTypes: []prompts.ReflectionTaskType{{
			Label:     "iterating on docgen output",
			Frequency: 3,
			Evidence: []prompts.ReflectionEvidence{
				{SessionID: "11111111-2222-3333-4444-555555555555",
					Quote: "Why is the schema page truncated?", WhatHappened: "schema docs missing"},
			},
		}},
		Frictions: []prompts.ReflectionFriction{{
			Label:     "stale autogen pages keep failing CI",
			Frequency: 2,
			Severity:  "medium",
			Evidence: []prompts.ReflectionEvidence{
				{SessionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
					Quote: "make docs-check fails again", WhatHappened: "rebuild needed"},
			},
		}},
		WorkflowChange: "Run make docs-check before pushing.",
	}, "stored-card", now.UnixMilli())

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/digests")
	for _, want := range []string{
		"Run make docs-check before pushing.", // workflow change
		"iterating on docgen output",          // task type label
		"stale autogen pages keep failing CI", // friction label
		"medium",                              // severity
		"×3", "×2",                            // frequency
		`href="/sessions/11111111-2222-3333-4444-555555555555"`,
		`href="/sessions/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"`,
		"11111111", "aaaaaaaa", // short ids
		"Why is the schema page truncated?", // quote
	} {
		if !strings.Contains(body, want) {
			t.Errorf("digest card missing %q\n%s", want, body)
		}
	}
	// Card title falls back to "digest #ID" — no period info is
	// persisted on the row.
	wantTitle := "digest #" + strconv.FormatInt(id, 10)
	if !strings.Contains(body, wantTitle) {
		t.Errorf("expected card title %q, got:\n%s", wantTitle, body)
	}
	// id link round-trips back to the same page with a fragment.
	if !strings.Contains(body, `#digest-`) {
		t.Errorf("expected per-card id anchor, got:\n%s", body)
	}
}

func TestDigestsPage_NewestFirst(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)

	older := prompts.ReflectionResult{
		WorkflowChange: "OLDER digest workflow change marker.",
	}
	newer := prompts.ReflectionResult{
		WorkflowChange: "NEWER digest workflow change marker.",
	}
	seedWeeklyDigest(t, st, older, "older", time.Date(2026, 4, 14, 6, 0, 0, 0, time.UTC).UnixMilli())
	seedWeeklyDigest(t, st, newer, "newer", time.Date(2026, 4, 21, 6, 0, 0, 0, time.UTC).UnixMilli())

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/digests")
	newerIdx := strings.Index(body, "NEWER digest workflow change marker.")
	olderIdx := strings.Index(body, "OLDER digest workflow change marker.")
	if newerIdx < 0 || olderIdx < 0 {
		t.Fatalf("both markers should appear; newerIdx=%d olderIdx=%d", newerIdx, olderIdx)
	}
	if newerIdx >= olderIdx {
		t.Errorf("newer digest should appear before older; newer=%d older=%d", newerIdx, olderIdx)
	}
}

func TestDigestsPage_RawBodyFallback(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)

	// Hand-write a malformed body directly so the ReflectionResult
	// unmarshal fails. The page must still render the card with the
	// raw body in a collapsible details — never blank out the whole
	// page.
	tx, _ := st.DB().Begin()
	_, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		Kind:        store.LLMKindReflectWeekly,
		Model:       "old-model",
		PromptHash:  "bad-shape",
		Body:        "not json at all { malformed",
		CreatedAtMs: time.Date(2026, 4, 21, 6, 0, 0, 0, time.UTC).UnixMilli(),
	})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("save: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/digests")
	if status != http.StatusOK {
		t.Errorf("page should still render; got %d", status)
	}
	if !strings.Contains(body, "unparseable body") {
		t.Errorf("expected raw-body fallback, got:\n%s", body)
	}
	if !strings.Contains(body, "not json at all") {
		t.Errorf("raw body should be surfaced, got:\n%s", body)
	}
}

func TestDigestsPage_RespectsLimitParam(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	// Vary the prompt-hash salt per iteration so SaveLLMOutput's
	// dedup (kind, prompt_hash) doesn't collapse the inserts to one
	// row.
	for i := range 5 {
		seedWeeklyDigest(t, st, prompts.ReflectionResult{
			WorkflowChange: "marker " + string(rune('a'+i)),
		}, "limit-"+strconv.Itoa(i),
			time.Date(2026, 4, 14, i, 0, 0, 0, time.UTC).UnixMilli())
	}
	base, stop := startTestServer(t, st)
	defer stop()

	// Default limit (26) shows all 5.
	_, full := fetch(t, base+"/digests")
	if got := strings.Count(full, "marker "); got < 5 {
		t.Errorf("default limit should show all 5; got %d", got)
	}
	// limit=2 narrows.
	_, narrow := fetch(t, base+"/digests?limit=2")
	if got := strings.Count(narrow, "marker "); got != 2 {
		t.Errorf("limit=2 should show 2; got %d", got)
	}
}
