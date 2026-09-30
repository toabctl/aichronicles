package skills

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
)

// seedSkillLoad plants one session (ended at endedMs, or still
// running when endedMs is 0) holding one skill_load extraction on an
// event at loadMs.
func seedSkillLoad(t *testing.T, db *sql.DB, skill string, loadMs, endedMs int64) {
	t.Helper()
	sess := uuid.NewString()
	var ended any
	if endedMs > 0 {
		ended = endedMs
	}
	eid := uuid.Must(uuid.NewV7()).String()
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, 'claude-code', ?)`, []any{sess, "src-" + sess}},
		{`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		  VALUES (?, ?, 'claude-code', ?, ?, ?, '{}')`, []any{eid, time.Now().UnixNano(), "src-" + sess, loadMs, loadMs}},
		{`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms) VALUES (?, ?, 'claude-code', 'tool_use', ?)`, []any{eid, sess, loadMs}},
		{`INSERT INTO extractions(event_id, session_id, kind, value) VALUES (?, ?, ?, ?)`, []any{eid, sess, events.ExtractionKindSkillLoad, skill}},
		// The insert trigger derives ended_at from the event; pin it
		// to the fixture's value (NULL = still running).
		{`UPDATE sessions SET ended_at_ms = ? WHERE id = ?`, []any{ended, sess}},
	} {
		if _, err := db.Exec(stmt.q, stmt.args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt.q)
		}
	}
}

// TestLoadInvoked_WindowsOnLoadTime is the regression gate for the
// invoked-skills window: it filtered on the session's ended_at_ms, so
// an old load in a recently-ended session counted, a load in a
// running session never did, and the counts disagreed with
// LoadSkillImpact's TotalLoads for the same since_ms.
func TestLoadInvoked_WindowsOnLoadTime(t *testing.T) {
	t.Parallel()
	st, err := store.OpenMigrate(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db := st.DB()

	now := time.Now().UnixMilli()
	day := int64(24 * time.Hour / time.Millisecond)
	since := now - 30*day
	seedSkillLoad(t, db, "old-load", now-60*day, now-day/24) // loaded outside, session ended inside
	seedSkillLoad(t, db, "live-load", now-day, 0)            // loaded inside, session still running
	seedSkillLoad(t, db, "recent", now-2*day, now-2*day+1000)
	seedSkillLoad(t, db, "recent", now-3*day, now-3*day+1000)

	got, err := LoadInvoked(context.Background(), db, since)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, s := range got {
		counts[s.Name] = s.Count
	}
	want := map[string]int{"recent": 2, "live-load": 1}
	if len(counts) != len(want) || counts["recent"] != 2 || counts["live-load"] != 1 {
		t.Errorf("invoked counts: got %v, want %v", counts, want)
	}

	impact, err := store.LoadSkillImpact(context.Background(), db, since, 0, store.SkillImpactLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range impact {
		if counts[s.Name] != s.TotalLoads {
			t.Errorf("skill %s: invoked count %d != impact total_loads %d for the same window",
				s.Name, counts[s.Name], s.TotalLoads)
		}
	}
	if len(impact) != len(counts) {
		t.Errorf("impact covers %d skills, invoked %d", len(impact), len(counts))
	}
}
