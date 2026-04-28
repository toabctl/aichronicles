package store

import (
	"strings"
	"testing"
)

// seedSession inserts a sessions row. Callers can pass cwd="" to
// get a NULL cwd, which exercises the "no anchor" code path in
// LoadCandidatePriorSessions.
func seedSession(t *testing.T, s *Store, id, cwd string, startedAt, endedAt int64) {
	t.Helper()
	srcSess := "src-" + id
	if cwd == "" {
		if _, err := s.DB().Exec(
			`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms)
			 VALUES (?, 'claude-code', ?, ?, ?)`,
			id, srcSess, startedAt, endedAt,
		); err != nil {
			t.Fatalf("seed session: %v", err)
		}
		return
	}
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms, cwd)
		 VALUES (?, 'claude-code', ?, ?, ?, ?)`,
		id, srcSess, startedAt, endedAt, cwd,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func seedSummaryTopic(t *testing.T, s *Store, sessionID, topic string, ts int64) {
	t.Helper()
	body := `{"topic":"` + topic + `","what_was_done":[],"unresolved":[]}`
	if _, err := s.DB().Exec(
		`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms)
		 VALUES (?, 'summary', ?, ?, 'fake-model', ?)`,
		sessionID, body, "h-"+sessionID, ts,
	); err != nil {
		t.Fatalf("seed summary: %v", err)
	}
}

func TestLoadCandidatePriorSessions_FiltersByCwdAndStart(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	const cwdA = "/repo/a"
	const cwdB = "/repo/b"
	const baseTs = int64(1_700_000_000_000)

	// anchor at t=10s in cwdA
	const anchor = "00000000-0000-0000-0000-0000000000aa"
	seedSession(t, s, anchor, cwdA, baseTs+10_000, baseTs+20_000)

	// candidate 1: same cwd, ended before anchor started — INCLUDE
	const cand1 = "00000000-0000-0000-0000-0000000000c1"
	seedSession(t, s, cand1, cwdA, baseTs-3*60_000, baseTs-2*60_000)
	seedSummaryTopic(t, s, cand1, "fixed the migration", baseTs-1*60_000)

	// candidate 2: same cwd, ended even earlier — INCLUDE (newer-first ordering)
	const cand2 = "00000000-0000-0000-0000-0000000000c2"
	seedSession(t, s, cand2, cwdA, baseTs-10*60_000, baseTs-9*60_000)
	seedSummaryTopic(t, s, cand2, "first migration attempt", baseTs-8*60_000)

	// candidate 3: different cwd — EXCLUDE
	const cand3 = "00000000-0000-0000-0000-0000000000c3"
	seedSession(t, s, cand3, cwdB, baseTs-5*60_000, baseTs-4*60_000)

	// candidate 4: same cwd but ended AFTER anchor started — EXCLUDE (future)
	const cand4 = "00000000-0000-0000-0000-0000000000c4"
	seedSession(t, s, cand4, cwdA, baseTs+30_000, baseTs+40_000)

	// candidate 5: same cwd, no summary — INCLUDE with empty topic
	const cand5 = "00000000-0000-0000-0000-0000000000c5"
	seedSession(t, s, cand5, cwdA, baseTs-1*60_000, baseTs-30_000)

	got, err := LoadCandidatePriorSessions(t.Context(), s.DB(), anchor, 10)
	if err != nil {
		t.Fatalf("LoadCandidatePriorSessions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 candidates, got %d: %+v", len(got), got)
	}
	// Newest-first: cand5 (ended -30s) > cand1 (-2m) > cand2 (-9m)
	if got[0].ID != cand5 {
		t.Errorf("got[0]: %q want %q", got[0].ID, cand5)
	}
	if got[0].Topic != "" {
		t.Errorf("cand5 has no summary, expected empty topic, got %q", got[0].Topic)
	}
	if got[1].ID != cand1 || got[1].Topic != "fixed the migration" {
		t.Errorf("got[1]: %+v want id=%s topic=fixed...", got[1], cand1)
	}
	if got[2].ID != cand2 || got[2].Topic != "first migration attempt" {
		t.Errorf("got[2]: %+v want id=%s topic=first...", got[2], cand2)
	}
}

func TestLoadCandidatePriorSessions_NoCwdReturnsEmpty(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const anchor = "00000000-0000-0000-0000-0000000000ab"
	// Session with NULL cwd.
	seedSession(t, s, anchor, "", 1_700_000_000_000, 1_700_000_010_000)
	// Add a candidate that WOULD match if anchor had a cwd.
	const cand = "00000000-0000-0000-0000-0000000000cd"
	seedSession(t, s, cand, "/some/path", 1_699_000_000_000, 1_699_000_010_000)

	got, err := LoadCandidatePriorSessions(t.Context(), s.DB(), anchor, 10)
	if err != nil {
		t.Fatalf("LoadCandidatePriorSessions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 candidates when anchor has no cwd, got %v", got)
	}
}

func TestSaveSessionLinks_RoundTripsAndOverwrites(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	const a = "00000000-0000-0000-0000-00000000000a"
	const b = "00000000-0000-0000-0000-00000000000b"
	const c = "00000000-0000-0000-0000-00000000000c"
	for _, id := range []string{a, b, c} {
		seedSession(t, s, id, "/repo", 1_700_000_000_000, 1_700_000_010_000)
	}

	// First write: a → b (builds_on), a → c (related)
	links1 := []SessionLink{
		{ToSessionID: b, Kind: SessionLinkBuildsOn, Rationale: "extends b's API change"},
		{ToSessionID: c, Kind: SessionLinkRelated, Rationale: "touched the same files"},
	}
	if err := SaveSessionLinks(t.Context(), s.DB(), a, links1); err != nil {
		t.Fatalf("save 1: %v", err)
	}

	got, err := LoadSessionLinksFrom(t.Context(), s.DB(), a)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 links, got %d: %+v", len(got), got)
	}
	// Canonical order: builds_on (1) before related (4)
	if got[0].Kind != SessionLinkBuildsOn || got[0].ToSessionID != b {
		t.Errorf("got[0] = %+v, want builds_on→b", got[0])
	}
	if got[1].Kind != SessionLinkRelated || got[1].ToSessionID != c {
		t.Errorf("got[1] = %+v, want related→c", got[1])
	}
	if got[0].Rationale != "extends b's API change" {
		t.Errorf("rationale lost: %q", got[0].Rationale)
	}

	// Reverse direction: b should see one incoming link from a.
	incoming, err := LoadSessionLinksTo(t.Context(), s.DB(), b)
	if err != nil {
		t.Fatalf("load to: %v", err)
	}
	if len(incoming) != 1 || incoming[0].FromSessionID != a {
		t.Errorf("incoming to b: got %+v, want 1 link from a", incoming)
	}

	// Second write replaces: a → b only (repeats_failure_of)
	links2 := []SessionLink{
		{ToSessionID: b, Kind: SessionLinkRepeatsFailureOf, Rationale: "same ENOENT"},
	}
	if err := SaveSessionLinks(t.Context(), s.DB(), a, links2); err != nil {
		t.Fatalf("save 2: %v", err)
	}

	got2, err := LoadSessionLinksFrom(t.Context(), s.DB(), a)
	if err != nil {
		t.Fatalf("load 2: %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("expected 1 link after replace, got %d: %+v", len(got2), got2)
	}
	if got2[0].Kind != SessionLinkRepeatsFailureOf {
		t.Errorf("kind: got %q, want repeats_failure_of", got2[0].Kind)
	}
	// The old a→c link should be gone.
	incomingC, err := LoadSessionLinksTo(t.Context(), s.DB(), c)
	if err != nil {
		t.Fatalf("load to c: %v", err)
	}
	if len(incomingC) != 0 {
		t.Errorf("c still has incoming link after replace: %+v", incomingC)
	}
}

func TestSaveSessionLinks_RejectsBadInput(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const a = "00000000-0000-0000-0000-0000000000a1"
	const b = "00000000-0000-0000-0000-0000000000b1"
	for _, id := range []string{a, b} {
		seedSession(t, s, id, "/r", 1_700_000_000_000, 1_700_000_010_000)
	}

	cases := []struct {
		name    string
		link    SessionLink
		wantSub string
	}{
		{"empty to", SessionLink{ToSessionID: "", Kind: SessionLinkBuildsOn}, "to_session_id is empty"},
		{"self-link", SessionLink{ToSessionID: a, Kind: SessionLinkBuildsOn}, "self-link"},
		{"bad kind", SessionLink{ToSessionID: b, Kind: "junk"}, "invalid kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SaveSessionLinks(t.Context(), s.DB(), a, []SessionLink{tc.link})
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestSaveSessionLinks_EmptyClearsExisting(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const a = "00000000-0000-0000-0000-0000000000a2"
	const b = "00000000-0000-0000-0000-0000000000b2"
	for _, id := range []string{a, b} {
		seedSession(t, s, id, "/r", 1_700_000_000_000, 1_700_000_010_000)
	}

	if err := SaveSessionLinks(t.Context(), s.DB(), a, []SessionLink{
		{ToSessionID: b, Kind: SessionLinkBuildsOn},
	}); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	// Clearing.
	if err := SaveSessionLinks(t.Context(), s.DB(), a, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err := LoadSessionLinksFrom(t.Context(), s.DB(), a)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty after clear, got %+v", got)
	}
}
