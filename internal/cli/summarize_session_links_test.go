package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// seedPriorSummarizedSession seeds a prior same-cwd session that
// has already been summarized — exactly the shape
// LoadCandidatePriorSessions returns to the summarize prompt.
func seedPriorSummarizedSession(t *testing.T, s *store.Store, id, cwd, topic string, ts int64) {
	t.Helper()
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms, cwd)
		 VALUES (?, 'claude-code', ?, ?, ?, ?)`,
		id, "src-"+id, ts-60_000, ts, cwd,
	); err != nil {
		t.Fatalf("seed prior session: %v", err)
	}
	body, _ := json.Marshal(prompts.SummaryResult{
		Topic:        topic,
		WhatWasDone:  []string{"x"},
		Unresolved:   []string{},
		KeyFiles:     []string{},
		Links:        []prompts.LinkAnnotation{},
		SessionLinks: []prompts.SessionLinkAnnotation{},
	})
	if _, err := s.DB().Exec(
		`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms)
		 VALUES (?, 'summary', ?, ?, 'fake-model', ?)`,
		id, string(body), "h-"+id, ts,
	); err != nil {
		t.Fatalf("seed prior summary: %v", err)
	}
}

func TestRunSummarize_PersistsValidSessionLinks(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)

	// Seed two prior summarized sessions in the SAME cwd as the
	// fixture from seedSessionForSummarize ("/work/systemd"), each
	// ending well before the current session started.
	const priorA = "11111111-1111-1111-1111-1111111111aa"
	const priorB = "22222222-2222-2222-2222-2222222222bb"
	startedAt := time.Now().UTC().UnixMilli() - 60*60*1000
	seedPriorSummarizedSession(t, s, priorA, "/work/systemd", "earlier socket-activation work", startedAt)
	seedPriorSummarizedSession(t, s, priorB, "/work/systemd", "an unrelated thread", startedAt-10_000)

	// LLM emits one valid link to priorA and one fabricated id
	// (must be silently dropped) and one bad-kind entry (also
	// dropped). The fake forces this exact tool input.
	toolInput, _ := json.Marshal(prompts.SummaryResult{
		Topic:       "follow-up on socket activation",
		WhatWasDone: []string{"reproduced the LISTEN_FDS issue"},
		Unresolved:  []string{},
		KeyFiles:    []string{},
		Links:       []prompts.LinkAnnotation{},
		Subagents:   []prompts.SubagentSummary{},
		SessionLinks: []prompts.SessionLinkAnnotation{
			{ToSessionID: priorA, Kind: store.SessionLinkBuildsOn, Rationale: "extends the LISTEN_FDS investigation"},
			{ToSessionID: "ffffffff-ffff-ffff-ffff-ffffffffffff", Kind: store.SessionLinkRelated, Rationale: "fabricated id"},
			{ToSessionID: priorB, Kind: "junk-kind", Rationale: "invalid kind"},
		},
	})
	f := &fakeLLM{toolInput: toolInput}

	var out bytes.Buffer
	if _, err := RunSummarize(context.Background(), apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		SummarizeOptions{SessionID: sessID}, &out); err != nil {
		t.Fatalf("RunSummarize: %v", err)
	}

	// Only the valid link should land.
	got, err := store.LoadSessionLinksFrom(t.Context(), s.DB(), sessID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 persisted link (others dropped), got %d: %+v", len(got), got)
	}
	if got[0].ToSessionID != priorA || got[0].Kind != store.SessionLinkBuildsOn {
		t.Errorf("got %+v, want builds_on→%s", got[0], priorA)
	}
	if !strings.Contains(got[0].Rationale, "LISTEN_FDS") {
		t.Errorf("rationale lost: %q", got[0].Rationale)
	}

	// And the candidate stanza must have been rendered into the
	// prompt — otherwise the LLM had no way to know about priorA.
	body := f.lastReq.Messages[0].Content
	if !strings.Contains(body, "Possibly-related prior sessions") {
		t.Errorf("candidate stanza missing from prompt:\n%s", body)
	}
	if !strings.Contains(body, priorA) {
		t.Errorf("priorA id missing from prompt:\n%s", body)
	}
}

func TestRunSummarize_NoCandidatesMeansNoLinkRows(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	// No prior sessions seeded — the candidate list is empty, the
	// stanza is omitted, but the LLM (out of contract) emits a link
	// anyway. It must be dropped.
	toolInput, _ := json.Marshal(prompts.SummaryResult{
		Topic:       "rogue",
		WhatWasDone: []string{"x"},
		Unresolved:  []string{},
		KeyFiles:    []string{},
		Links:       []prompts.LinkAnnotation{},
		Subagents:   []prompts.SubagentSummary{},
		SessionLinks: []prompts.SessionLinkAnnotation{
			{ToSessionID: "deadbeef-dead-beef-dead-beefdeadbeef", Kind: store.SessionLinkRelated, Rationale: "out of band"},
		},
	})
	f := &fakeLLM{toolInput: toolInput}

	if _, err := RunSummarize(context.Background(), apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		SummarizeOptions{SessionID: sessID}, &bytes.Buffer{}); err != nil {
		t.Fatalf("RunSummarize: %v", err)
	}

	got, err := store.LoadSessionLinksFrom(t.Context(), s.DB(), sessID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 link rows when no candidates were offered, got %+v", got)
	}
	body := f.lastReq.Messages[0].Content
	if strings.Contains(body, "Possibly-related prior sessions") {
		t.Errorf("candidate stanza should be omitted when no candidates exist:\n%s", body)
	}
}

func TestRunSummarize_CacheHitReprojectsLinks(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)

	const priorA = "33333333-3333-3333-3333-333333333333"
	startedAt := time.Now().UTC().UnixMilli() - 60*60*1000
	seedPriorSummarizedSession(t, s, priorA, "/work/systemd", "earlier work", startedAt)

	// First call writes the cache + link rows.
	toolInput, _ := json.Marshal(prompts.SummaryResult{
		Topic:       "t",
		WhatWasDone: []string{"x"},
		Unresolved:  []string{},
		KeyFiles:    []string{},
		Links:       []prompts.LinkAnnotation{},
		Subagents:   []prompts.SubagentSummary{},
		SessionLinks: []prompts.SessionLinkAnnotation{
			{ToSessionID: priorA, Kind: store.SessionLinkRelated, Rationale: "topic overlap"},
		},
	})
	f := &fakeLLM{toolInput: toolInput}
	if _, err := RunSummarize(context.Background(), apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		SummarizeOptions{SessionID: sessID}, &bytes.Buffer{}); err != nil {
		t.Fatalf("first: %v", err)
	}

	// Wipe the link rows out from under us — simulate the case where
	// the user ran summarize before the links table existed.
	if _, err := s.DB().Exec(`DELETE FROM session_links`); err != nil {
		t.Fatalf("wipe: %v", err)
	}

	// Second call: cache hit, must NOT call the LLM, but MUST
	// reproject the links.
	if _, err := RunSummarize(context.Background(), apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		SummarizeOptions{SessionID: sessID}, &bytes.Buffer{}); err != nil {
		t.Fatalf("second: %v", err)
	}
	if f.called != 1 {
		t.Errorf("LLM call count: got %d, want 1 (cache hit)", f.called)
	}
	got, err := store.LoadSessionLinksFrom(t.Context(), s.DB(), sessID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got[0].ToSessionID != priorA {
		t.Errorf("expected reprojected link to %s, got %+v", priorA, got)
	}
}
