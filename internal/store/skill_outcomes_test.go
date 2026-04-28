package store

import (
	"testing"
)

func TestLoadSkillImpact_PositiveAndNegativeRows(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Three skills, one session each so post-load failures don't
	// cross-contaminate (the 10-min window is in-session only,
	// but if we put every load in one session the failure
	// stream from the noisy/broken skills would land within the
	// clean skill's lookahead window too):
	//   "clean"  — 3 loads, 0 failures (100% success)
	//   "noisy"  — 4 loads, 2 failures (50%)
	//   "broken" — 2 loads, 2 failures (0%)
	const baseTs = int64(1_700_000_000_000) // arbitrary epoch ms

	type loadSpec struct {
		offsetMs     int64
		failureAfter int64 // 0 = no failure within window
	}
	cases := []struct {
		skill   string
		session string
		loads   []loadSpec
	}{
		// Loads spaced > 10min apart per skill so each load's
		// lookahead window only sees its OWN dedicated failure (or
		// none). Failures at 30s after the load are well within
		// the default 10-minute window.
		{"clean", "00000000-0000-0000-0000-000000000001", []loadSpec{
			{0, 0}, {2_000_000, 0}, {4_000_000, 0},
		}},
		{"noisy", "00000000-0000-0000-0000-000000000002", []loadSpec{
			{0, 30_000},
			{2_000_000, 0},
			{4_000_000, 30_000},
			{6_000_000, 0},
		}},
		{"broken", "00000000-0000-0000-0000-000000000003", []loadSpec{
			{0, 60_000}, {2_000_000, 60_000},
		}},
	}

	seq := int64(1)
	for ci, c := range cases {
		// Seed the session row before any FK insert.
		if _, err := s.DB().Exec(
			`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms)
			 VALUES (?, 'claude-code', ?, ?, ?)`,
			c.session, c.skill+"-src", baseTs, baseTs+24*60*60*1000,
		); err != nil {
			t.Fatalf("seed session %q: %v", c.skill, err)
		}
		for li, sp := range c.loads {
			ts := baseTs + sp.offsetMs
			loadEvent := mkUUIDLikeID(t, c.skill+"-load", ci*100+li)
			if _, err := s.DB().Exec(
				`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
				 VALUES (?, ?, 'claude-code', ?, ?, ?, '{}')`,
				loadEvent, seq, c.skill+"-src", ts, ts,
			); err != nil {
				t.Fatalf("raw envelope: %v", err)
			}
			seq++
			if _, err := s.DB().Exec(
				`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, content_text)
				 VALUES (?, ?, 'claude-code', 'system_message', ?, '')`,
				loadEvent, c.session, ts,
			); err != nil {
				t.Fatalf("event: %v", err)
			}
			if _, err := s.DB().Exec(
				`INSERT INTO extractions(event_id, session_id, kind, value)
				 VALUES (?, ?, 'skill_load', ?)`,
				loadEvent, c.session, c.skill,
			); err != nil {
				t.Fatalf("extraction: %v", err)
			}
			if sp.failureAfter > 0 {
				failTs := ts + sp.failureAfter
				failEvent := mkUUIDLikeID(t, c.skill+"-fail", ci*100+li)
				if _, err := s.DB().Exec(
					`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
					 VALUES (?, ?, 'claude-code', ?, ?, ?, '{}')`,
					failEvent, seq, c.skill+"-src", failTs, failTs,
				); err != nil {
					t.Fatalf("raw failure envelope: %v", err)
				}
				seq++
				if _, err := s.DB().Exec(
					`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, content_text)
					 VALUES (?, ?, 'claude-code', 'tool_failure', ?, 'boom')`,
					failEvent, c.session, failTs,
				); err != nil {
					t.Fatalf("failure event: %v", err)
				}
			}
		}
	}

	rows, err := LoadSkillImpact(t.Context(), s.DB(), baseTs-1, 0, SkillImpactLimits{})
	if err != nil {
		t.Fatalf("LoadSkillImpact: %v", err)
	}
	got := map[string]SkillImpact{}
	for _, r := range rows {
		got[r.Name] = r
	}
	for _, want := range []struct {
		name        string
		total, fail int
		rate        float64
	}{
		{"clean", 3, 0, 1.0},
		{"noisy", 4, 2, 0.5},
		{"broken", 2, 2, 0.0},
	} {
		r, ok := got[want.name]
		if !ok {
			t.Errorf("skill %q missing from impact rows: %+v", want.name, rows)
			continue
		}
		if r.TotalLoads != want.total {
			t.Errorf("%q total: got %d, want %d", want.name, r.TotalLoads, want.total)
		}
		if r.FailedLoads != want.fail {
			t.Errorf("%q failed: got %d, want %d", want.name, r.FailedLoads, want.fail)
		}
		if absDelta(r.SuccessRate, want.rate) > 1e-9 {
			t.Errorf("%q rate: got %v, want %v", want.name, r.SuccessRate, want.rate)
		}
	}

	// Order is most-loaded first: noisy (4), clean (3), broken (2).
	wantOrder := []string{"noisy", "clean", "broken"}
	for i, n := range wantOrder {
		if rows[i].Name != n {
			t.Errorf("order: rows[%d]=%q, want %q (full order: %v)",
				i, rows[i].Name, n, namesOf(rows))
		}
	}
}

func TestLoadSkillImpact_EmptyStoreReturnsEmpty(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	rows, err := LoadSkillImpact(t.Context(), s.DB(), 0, 0, SkillImpactLimits{})
	if err != nil {
		t.Fatalf("LoadSkillImpact: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected empty result on empty store, got %v", rows)
	}
}

func TestLoadSkillImpact_RespectsSinceCutoff(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	const sess = "00000000-0000-0000-0000-000000000002"
	const baseTs = int64(1_700_000_000_000)
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms)
		 VALUES (?, 'claude-code', 'src', ?, ?)`,
		sess, baseTs, baseTs+24*60*60*1000,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// One load way before the cutoff, one well after.
	for i, ts := range []int64{baseTs, baseTs + 7*24*60*60*1000} {
		eid := mkUUIDLikeID(t, "load", i)
		if _, err := s.DB().Exec(
			`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
			 VALUES (?, ?, 'claude-code', 'src', ?, ?, '{}')`,
			eid, int64(i+1), ts, ts,
		); err != nil {
			t.Fatalf("raw %d: %v", i, err)
		}
		if _, err := s.DB().Exec(
			`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, content_text)
			 VALUES (?, ?, 'claude-code', 'system_message', ?, '')`,
			eid, sess, ts,
		); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if _, err := s.DB().Exec(
			`INSERT INTO extractions(event_id, session_id, kind, value)
			 VALUES (?, ?, 'skill_load', 'window-test')`,
			eid, sess,
		); err != nil {
			t.Fatalf("ext %d: %v", i, err)
		}
	}

	// Cutoff between the two loads — only the late one counts.
	rows, err := LoadSkillImpact(t.Context(), s.DB(),
		baseTs+24*60*60*1000, 0, SkillImpactLimits{})
	if err != nil {
		t.Fatalf("LoadSkillImpact: %v", err)
	}
	if len(rows) != 1 || rows[0].TotalLoads != 1 {
		t.Errorf("since cutoff should drop the early load: %+v", rows)
	}
}

// mkUUIDLikeID generates a stable, unique-per-test event id from
// (kind, idx) so the test doesn't pull in a uuid dep just for
// fixture data.
func mkUUIDLikeID(t *testing.T, kind string, idx int) string {
	t.Helper()
	return kind + "-" + uuidLikePadded(idx)
}

// uuidLikePadded returns a 36-char uuid-shaped string from idx so
// raw_envelopes' TEXT column gets stable, sortable values.
func uuidLikePadded(idx int) string {
	return "00000000-0000-0000-0000-" + intToHex12(idx)
}

func intToHex12(n int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 12)
	for i := 11; i >= 0; i-- {
		out[i] = hex[n&0xf]
		n >>= 4
	}
	return string(out)
}

func absDelta(a, b float64) float64 {
	if a < b {
		return b - a
	}
	return a - b
}

func namesOf(rows []SkillImpact) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}
