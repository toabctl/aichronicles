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

// TestRunPropose_ChallengeFlagPersistsWithKindChallenge confirms
// the --challenge branch writes its row under kind=challenge,
// not kind=propose. Critical contract — the listing/cache layer
// uses the kind discriminator to keep the two output streams
// from colliding on prompt_hash collisions or rendering paths.
func TestRunPropose_ChallengeFlagPersistsWithKindChallenge(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 3)

	// LLM returns a real challenge so the rendered output is
	// substantive; the default synthMinimalToolInput emits an
	// empty list which would parse but read as "no challenges".
	body, _ := json.Marshal(prompts.ChallengeResult{
		Challenges: []prompts.Challenge{{
			Title:            "wire-structured-logs",
			Problem:          "wire structured slog calls through internal/daemon so /v1/ingest emits one line per envelope",
			Why:              "the user repeatedly grepped raw stderr in the ingest sessions; structured logs would give them journalctl filtering",
			GroundedIn:       []string{"sess-1"},
			Effort:           "small",
			SuccessLooksLike: "journalctl -u aichronicles | jq emits one line per envelope with content_hash",
		}},
	})
	f := &fakeLLM{toolInput: body}

	var out bytes.Buffer
	id, err := RunPropose(context.Background(), s, apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		ProposeOptions{
			Since:     10 * time.Hour,
			Limit:     10,
			Challenge: true,
		}, &out)
	if err != nil {
		t.Fatalf("RunPropose --challenge: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero row id")
	}

	var kind string
	_ = s.DB().QueryRow(`SELECT kind FROM llm_outputs WHERE id = ?`, id).Scan(&kind)
	if kind != string(store.LLMKindChallenge) {
		t.Errorf("kind: got %q, want %q", kind, store.LLMKindChallenge)
	}

	// Rendered output uses the challenge layout, not propose.
	rendered := out.String()
	for _, want := range []string{
		"Proposed challenges",
		"wire-structured-logs",
		"wire structured slog calls",
		"effort=small",
		"anchors: sess-1",
		"success: journalctl",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered output missing %q:\n%s", want, rendered)
		}
	}
}

// TestRunPropose_ChallengeAndProposeCacheIndependently confirms
// that running propose and propose --challenge against the SAME
// session set produces TWO llm_outputs rows (different kinds, no
// cache collision). The shared prompt_hash column is unique only
// per kind — easy thing to break with a sloppy refactor.
func TestRunPropose_ChallengeAndProposeCacheIndependently(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 3)
	newClient := func() (llm.Client, error) {
		return &fakeLLM{reply: "x"}, nil
	}

	if _, err := RunPropose(context.Background(), s, apiForStore(t, s), newClient,
		ProposeOptions{Since: 10 * time.Hour, Limit: 10},
		&bytes.Buffer{}); err != nil {
		t.Fatalf("propose: %v", err)
	}
	if _, err := RunPropose(context.Background(), s, apiForStore(t, s), newClient,
		ProposeOptions{Since: 10 * time.Hour, Limit: 10, Challenge: true},
		&bytes.Buffer{}); err != nil {
		t.Fatalf("challenge: %v", err)
	}

	var n int
	_ = s.DB().QueryRow(
		`SELECT COUNT(*) FROM llm_outputs WHERE kind IN ('propose','challenge')`,
	).Scan(&n)
	if n != 2 {
		t.Errorf("expected 2 rows (propose + challenge), got %d", n)
	}
}

// TestRunPropose_ChallengeReusesUnresolvedEnrichment proves the
// open-threads stanza ends up in the prompt the LLM sees. We
// plant a summary on a same-cwd prior session whose `unresolved`
// list carries a marker string, then assert the marker appears
// in the captured Request body.
func TestRunPropose_ChallengeReusesUnresolvedEnrichment(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 2)

	// Plant a prior session in the same cwd with an unresolved
	// item we can grep for. seedSessionsForMeta uses cwd
	// "/work/sess-meta-N"; the digest's first session ends up
	// with cwd "/work/sess-meta-1" (latest first).
	const priorCwd = "/work/sess-meta-1"
	const priorID = "00000000-0000-0000-0000-00000000c107"
	earlier := time.Now().Add(-3 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms, cwd)
		 VALUES (?, 'claude-code', 'src-c107', ?, ?, ?)`,
		priorID, earlier-3600_000, earlier, priorCwd,
	); err != nil {
		t.Fatalf("seed prior session: %v", err)
	}
	const marker = "MARKER_FOR_OPEN_THREADS"
	priorBody, _ := json.Marshal(prompts.SummaryResult{
		Topic:        "earlier exploration",
		WhatWasDone:  []string{"x"},
		Unresolved:   []string{marker + " — wire up the next thing"},
		KeyFiles:     []string{},
		Links:        []prompts.LinkAnnotation{},
		Subagents:    []prompts.SubagentSummary{},
		SessionLinks: []prompts.SessionLinkAnnotation{},
	})
	if _, err := s.DB().Exec(
		`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms)
		 VALUES (?, 'summary', ?, 'h-c107', 'fake-model', ?)`,
		priorID, string(priorBody), earlier,
	); err != nil {
		t.Fatalf("seed prior summary: %v", err)
	}

	f := &fakeLLM{}
	if _, err := RunPropose(context.Background(), s, apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		ProposeOptions{
			Since:     10 * time.Hour,
			Limit:     10,
			Challenge: true,
		}, &bytes.Buffer{}); err != nil {
		t.Fatalf("RunPropose --challenge: %v", err)
	}

	body := f.lastReq.Messages[0].Content
	if !strings.Contains(body, "Open threads observed") {
		t.Errorf("Open threads stanza missing from prompt:\n%s", body)
	}
	if !strings.Contains(body, marker) {
		t.Errorf("unresolved marker %q missing from prompt:\n%s", marker, body)
	}
}
