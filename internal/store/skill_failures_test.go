package store

import (
	"strings"
	"testing"
)

func TestLoadSkillFailures_ReturnsContextAroundFailure(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	const sess = "00000000-0000-0000-0000-000000000010"
	const baseTs = int64(1_700_000_000_000)
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms)
		 VALUES (?, 'claude-code', 'src', ?, ?)`,
		sess, baseTs, baseTs+24*60*60*1000,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Timeline:
	//   t+0s    skill_load my-skill
	//   t+10s   tool_use Bash (some setup)
	//   t+20s   tool_failure ("file not found: /etc/foo")
	//   t+30s   assistant_message ("retrying with sudo")
	type seed struct {
		offset  int64
		kind    string
		content string
		ext     string // skill_load extraction value when set
	}
	seeds := []seed{
		{0, "system_message", "skill loaded", "my-skill"},
		{10_000, "tool_use", "Bash: cat /etc/foo", ""},
		{20_000, "tool_failure", "file not found: /etc/foo", ""},
		{30_000, "assistant_message", "retrying with sudo", ""},
	}
	for i, sp := range seeds {
		eid := "ev-" + uuidLikePadded(i)
		ts := baseTs + sp.offset
		if _, err := s.DB().Exec(
			`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
			 VALUES (?, ?, 'claude-code', 'src', ?, ?, '{}')`,
			eid, int64(i+1), ts, ts,
		); err != nil {
			t.Fatalf("raw envelope: %v", err)
		}
		if _, err := s.DB().Exec(
			`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, content_text)
			 VALUES (?, ?, 'claude-code', ?, ?, ?)`,
			eid, sess, sp.kind, ts, sp.content,
		); err != nil {
			t.Fatalf("event: %v", err)
		}
		if sp.ext != "" {
			if _, err := s.DB().Exec(
				`INSERT INTO extractions(event_id, session_id, kind, value)
				 VALUES (?, ?, 'skill_load', ?)`,
				eid, sess, sp.ext,
			); err != nil {
				t.Fatalf("extraction: %v", err)
			}
		}
	}

	failures, err := LoadSkillFailures(t.Context(), s.DB(),
		"my-skill", baseTs-1, 0, 10)
	if err != nil {
		t.Fatalf("LoadSkillFailures: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(failures), failures)
	}
	f := failures[0]
	if f.SessionID != sess {
		t.Errorf("session_id: got %q, want %q", f.SessionID, sess)
	}
	if !strings.Contains(f.FailBody, "file not found") {
		t.Errorf("FailBody missing the failure message: %q", f.FailBody)
	}
	// Nearby should include the surrounding events as a timeline.
	for _, want := range []string{
		"[tool_use]",
		"Bash: cat /etc/foo",
		"[tool_failure]",
		"[assistant_message]",
		"retrying with sudo",
	} {
		if !strings.Contains(f.NearbyText, want) {
			t.Errorf("NearbyText missing %q\n--- got ---\n%s", want, f.NearbyText)
		}
	}
}

func TestLoadSkillFailures_NoFailureInWindowReturnsEmpty(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	const sess = "00000000-0000-0000-0000-000000000011"
	const baseTs = int64(1_700_000_000_000)
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms)
		 VALUES (?, 'claude-code', 'src', ?, ?)`,
		sess, baseTs, baseTs+24*60*60*1000,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Just a load, no failure.
	eid := "ev-clean"
	if _, err := s.DB().Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES (?, 1, 'claude-code', 'src', ?, ?, '{}')`,
		eid, baseTs, baseTs,
	); err != nil {
		t.Fatalf("raw: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, content_text)
		 VALUES (?, ?, 'claude-code', 'system_message', ?, '')`,
		eid, sess, baseTs,
	); err != nil {
		t.Fatalf("event: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO extractions(event_id, session_id, kind, value)
		 VALUES (?, ?, 'skill_load', 'lonely-skill')`,
		eid, sess,
	); err != nil {
		t.Fatalf("extraction: %v", err)
	}

	failures, err := LoadSkillFailures(t.Context(), s.DB(),
		"lonely-skill", baseTs-1, 0, 10)
	if err != nil {
		t.Fatalf("LoadSkillFailures: %v", err)
	}
	if len(failures) != 0 {
		t.Errorf("expected 0 failures, got %v", failures)
	}
}
