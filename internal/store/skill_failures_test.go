package store

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
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

// TestLoadSkillFailures_OneRowPerFailure is the regression gate for
// the load×failure fan-out: a failure preceded by two loads of the
// skill inside the window came back twice. It must appear once,
// attributed to the most recent load.
func TestLoadSkillFailures_OneRowPerFailure(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	t0 := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	seedSkillLoadAt(t, s, "sess-fanout", "flaky-skill", t0)
	seedSkillLoadAt(t, s, "sess-fanout", "flaky-skill", t0.Add(time.Minute))
	seedToolFailureAt(t, s, "sess-fanout", t0.Add(2*time.Minute))

	got, err := LoadSkillFailures(t.Context(), s.DB(), "flaky-skill", t0.Add(-time.Hour).UnixMilli(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows for one failure, want 1: %+v", len(got), got)
	}
	if want := t0.Add(time.Minute).UnixMilli(); got[0].LoadTsMs != want {
		t.Errorf("load_ts %d, want the most recent preceding load %d", got[0].LoadTsMs, want)
	}
}

// TestLoadSkillFailures_NearbyIsCenteredOnTheFailure pins the nearby
// window: with more than six events in the minute before a failure,
// the old "first six from fail-60s" pick dropped the failure's
// aftermath. The six closest events must straddle the failure.
func TestLoadSkillFailures_NearbyIsCenteredOnTheFailure(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	t0 := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	seedSkillLoadAt(t, s, "sess-busy", "busy-skill", t0)
	for i := range 8 { // eight events in the 50s before the failure
		seedNamedEvent(t, s, "sess-busy", "before-"+strconv.Itoa(i), t0.Add(time.Duration(i+1)*5*time.Second))
	}
	fail := t0.Add(50 * time.Second)
	seedToolFailureAt(t, s, "sess-busy", fail)
	seedNamedEvent(t, s, "sess-busy", "after-the-failure", fail.Add(2*time.Second))

	got, err := LoadSkillFailures(t.Context(), s.DB(), "busy-skill", t0.Add(-time.Hour).UnixMilli(), 0, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
	nearby := got[0].NearbyText
	for _, want := range []string{"Exit code 1", "after-the-failure", "before-7"} {
		if !strings.Contains(nearby, want) {
			t.Errorf("nearby missing %q:\n%s", want, nearby)
		}
	}
	if strings.Contains(nearby, "before-0") {
		t.Errorf("nearby kept the farthest event instead of the aftermath:\n%s", nearby)
	}
	if i, j := strings.Index(nearby, "before-7"), strings.Index(nearby, "after-the-failure"); i > j {
		t.Errorf("nearby not chronological:\n%s", nearby)
	}
}

func seedNamedEvent(t *testing.T, s *Store, sessionKey, content string, ts time.Time) {
	t.Helper()
	env := &events.Envelope{
		V: 1, EventID: uuid.Must(uuid.NewV7()).String(),
		SourceAgent: "claude-code", SourceSessionID: sessionKey,
		Kind: "assistant_message", Role: "assistant", TsSource: ts,
		ContentText: content, Payload: map[string]any{},
		Redaction: &events.Redaction{Applied: true},
	}
	withTx(t, s, func(tx *sql.Tx) {
		if _, _, err := IngestEnvelope(t.Context(), tx, env, []byte(`{"v":1}`), ts.UnixMilli()); err != nil {
			t.Fatalf("seed: %v", err)
		}
	})
}
