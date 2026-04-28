package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/ingest/extract"
)

func seedProposeLLMOutput(t *testing.T, st *store.Store) int64 {
	t.Helper()
	r, err := st.DB().Exec(
		`INSERT INTO llm_outputs(kind, model, prompt_hash, body, created_at_ms)
		 VALUES ('propose', 'fake-model', ?, '{}', ?)`,
		"h-"+t.Name(), time.Now().Add(-7*24*time.Hour).UnixMilli(),
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := r.LastInsertId()
	return id
}

func TestProposalsPage_Empty(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/proposals")
	if status != http.StatusOK {
		t.Fatalf("status: %d; body=%s", status, body)
	}
	for _, want := range []string{
		"Proposals",
		"No proposals recorded yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestProposalsPage_BucketsByLifecycle(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	now := time.Now()
	loID := seedProposeLLMOutput(t, st)
	proposedAt := now.Add(-7 * 24 * time.Hour).UnixMilli()
	appliedAt := now.Add(-6 * 24 * time.Hour).UnixMilli()
	const sessID = "00000000-0000-0000-0000-000000000aaa"

	// Seed a session + skill_load events to make
	// LoadProposalEffectiveness produce non-zero loads_after_apply
	// for the "applied-working" skill.
	if _, err := st.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms)
		 VALUES (?, 'claude-code', 'src-x', ?, ?)`,
		sessID, proposedAt, appliedAt+24*60*60*1000,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for _, name := range []string{"deploy-staging"} {
		seq := time.Now().UnixNano()
		evtID := name + "-evt"
		loadTs := appliedAt + 60_000 // 1 min after apply
		if _, err := st.DB().Exec(
			`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
			 VALUES (?, ?, 'claude-code', 'src-x', ?, ?, '{}')`,
			evtID, seq, loadTs, loadTs,
		); err != nil {
			t.Fatalf("envelope: %v", err)
		}
		if _, err := st.DB().Exec(
			`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms)
			 VALUES (?, ?, 'claude-code', 'system_message', ?)`,
			evtID, sessID, loadTs,
		); err != nil {
			t.Fatalf("event: %v", err)
		}
		if _, err := st.DB().Exec(
			`INSERT INTO extractions(event_id, session_id, kind, value)
			 VALUES (?, ?, ?, ?)`,
			evtID, sessID, extract.KindSkillLoad, name,
		); err != nil {
			t.Fatalf("extraction: %v", err)
		}
	}
	// "deploy-staging": applied + loaded (working bucket).
	if err := store.RecordProposedSkill(context.Background(), st.DB(),
		loID, "deploy-staging", proposedAt); err != nil {
		t.Fatalf("record working: %v", err)
	}
	if err := store.MarkProposedSkillApplied(context.Background(), st.DB(),
		loID, "deploy-staging", "/p/deploy-staging/SKILL.md", appliedAt); err != nil {
		t.Fatalf("apply working: %v", err)
	}
	// "fix-flake": applied but never loaded (unused bucket).
	if err := store.RecordProposedSkill(context.Background(), st.DB(),
		loID, "fix-flake", proposedAt); err != nil {
		t.Fatalf("record unused: %v", err)
	}
	if err := store.MarkProposedSkillApplied(context.Background(), st.DB(),
		loID, "fix-flake", "/p/fix-flake/SKILL.md", appliedAt); err != nil {
		t.Fatalf("apply unused: %v", err)
	}
	// "rejected-idea": never applied (not-applied bucket).
	if err := store.RecordProposedSkill(context.Background(), st.DB(),
		loID, "rejected-idea", proposedAt); err != nil {
		t.Fatalf("record not-applied: %v", err)
	}

	_, body := fetch(t, base+"/proposals")
	for _, want := range []string{
		"Applied · in use, working",
		"deploy-staging",
		"Applied · unused",
		"fix-flake",
		"Not applied",
		"rejected-idea",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n--- body ---\n%s", want, body)
		}
	}

	_ = ingest.KindUserPrompt // keep ingest import linked across test refactors
}
