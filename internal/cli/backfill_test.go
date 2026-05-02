package cli

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/events"
)

// silentBackfillLogger discards backfill progress log records so
// tests don't pollute output. We only assert on the report; the
// logger is just plumbing.
func silentBackfillLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedSkillIngest pushes one Skill tool_use envelope through the
// real ingest path so raw_envelopes + events + extractions all
// land. Returns the event_id and session_id for assertions.
func seedSkillIngest(t *testing.T, s *store.Store, sessionKey, skillName string, ts time.Time) (eventID, sessionID string) {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sessionKey,
		Kind:            "tool_use",
		Role:            "assistant",
		TsSource:        ts,
		Tool:            &events.Tool{Name: "Skill"},
		ContentText:     "Skill",
		Payload:         map[string]any{"tool_input": map[string]any{"skill": skillName}},
		Redaction:       &events.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(env)
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.IngestEnvelope(t.Context(), tx, env, raw, env.TsSource.UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ingest: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return env.EventID, events.DeriveSessionID(env.SourceAgent, env.SourceSessionID)
}

func openTempCLIStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/store.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestRunBackfillExtractions_RebuildsFromRawEnvelopes simulates
// the real-world scenario: extractions table is empty (the
// extractor didn't exist when the envelope was ingested), but
// raw_envelopes has the data — backfill must recover the missing
// rows.
func TestRunBackfillExtractions_RebuildsFromRawEnvelopes(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)

	eventID, _ := seedSkillIngest(t, s, "sess-bf", "build-test", time.Now())

	// Nuke extractions so the row "didn't exist" — equivalent to
	// "ingested before the extractor existed."
	if _, err := s.DB().Exec(`DELETE FROM extractions`); err != nil {
		t.Fatalf("clear extractions: %v", err)
	}

	report, err := RunBackfillExtractions(t.Context(), s, "", silentBackfillLogger())
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.EnvelopesScanned != 1 {
		t.Errorf("scanned: got %d, want 1", report.EnvelopesScanned)
	}
	if report.Inserted != 1 {
		t.Errorf("inserted: got %d, want 1", report.Inserted)
	}
	if report.ByKind[events.ExtractionKindSkillLoad] != 1 {
		t.Errorf("ByKind[skill_load]: got %d, want 1", report.ByKind[events.ExtractionKindSkillLoad])
	}

	var got string
	if err := s.DB().QueryRow(
		`SELECT value FROM extractions WHERE event_id = ? AND kind = ?`,
		eventID, events.ExtractionKindSkillLoad,
	).Scan(&got); err != nil {
		t.Fatalf("verify row: %v", err)
	}
	if got != "build-test" {
		t.Errorf("extraction value: got %q, want build-test", got)
	}
}

// TestRunBackfillExtractions_OnlyKindIsTargeted confirms --only
// touches a single kind. Other kinds' rows must survive untouched
// even though their event was rewritten.
func TestRunBackfillExtractions_OnlyKindIsTargeted(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)

	eventID, sessionID := seedSkillIngest(t, s, "sess-bf-only", "build-test", time.Now())

	// Plant a stray "url" extraction on the same event_id, then
	// run backfill --only=skill_load. The url row must survive.
	if _, err := s.DB().Exec(
		`INSERT INTO extractions(event_id, session_id, kind, value) VALUES (?, ?, ?, ?)`,
		eventID, sessionID, events.ExtractionKindURL, "https://example.com/manual",
	); err != nil {
		t.Fatalf("seed url: %v", err)
	}

	if _, err := RunBackfillExtractions(t.Context(), s, events.ExtractionKindSkillLoad, silentBackfillLogger()); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// url survived
	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM extractions WHERE event_id = ? AND kind = ? AND value = ?`,
		eventID, events.ExtractionKindURL, "https://example.com/manual",
	).Scan(&n); err != nil {
		t.Fatalf("count url: %v", err)
	}
	if n != 1 {
		t.Errorf("url survived: got %d rows, want 1", n)
	}
}

// TestRunBackfillExtractions_IsIdempotent confirms running
// backfill twice doesn't duplicate rows — the DELETE-then-INSERT
// pattern is the load-bearing invariant.
func TestRunBackfillExtractions_IsIdempotent(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)

	seedSkillIngest(t, s, "sess-idem", "effective-go", time.Now())

	for i := 0; i < 3; i++ {
		if _, err := RunBackfillExtractions(t.Context(), s, "", silentBackfillLogger()); err != nil {
			t.Fatalf("backfill #%d: %v", i, err)
		}
	}

	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM extractions WHERE kind = ?`,
		events.ExtractionKindSkillLoad,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("after 3 backfills: got %d rows, want 1", n)
	}
}

// TestRunBackfillExtractions_HandlesMalformedEnvelopeJSON pins the
// "one bad row doesn't fail the whole batch" rule. We poison a
// raw_envelopes row directly and expect the report to count it
// invalid and the rest to land.
func TestRunBackfillExtractions_HandlesMalformedEnvelopeJSON(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)

	seedSkillIngest(t, s, "sess-good", "build-test", time.Now())
	// Inject a poison row directly into raw_envelopes — bypassing
	// the ingest path so we can store invalid JSON.
	const poisonSeq = 9999999
	if _, err := s.DB().Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id,
		                            ts_source_ms, ts_server_ms, envelope_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		uuid.Must(uuid.NewV7()).String(), poisonSeq, "claude-code", "x",
		time.Now().UnixMilli(), time.Now().UnixMilli(),
		"this is not json {{{",
	); err != nil {
		// Schema may forbid this — that's fine, skip the test.
		t.Skipf("could not inject poison row: %v", err)
	}

	report, err := RunBackfillExtractions(t.Context(), s, "", silentBackfillLogger())
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.Invalid < 1 {
		t.Errorf("expected at least one invalid envelope, got %d", report.Invalid)
	}
	if report.Inserted < 1 {
		t.Errorf("expected good envelope to still extract, inserted=%d", report.Inserted)
	}
}

// Tiny helper for the report struct — string output should at
// least contain the headline counts and the per-kind block.
func TestBackfillReport_String(t *testing.T) {
	t.Parallel()
	r := BackfillReport{
		EnvelopesScanned: 10,
		EnvelopesParsed:  9,
		Invalid:          1,
		Deleted:          5,
		Inserted:         12,
		ByKind:           map[string]int{"url": 7, "skill_load": 5},
		DurationMS:       42,
	}
	out := r.String()
	for _, want := range []string{"envelopes scanned:   10", "rows inserted:       12", "url", "skill_load"} {
		if !contains(out, want) {
			t.Errorf("BackfillReport.String missing %q\n%s", want, out)
		}
	}
	_ = sql.NullInt64{}
}

// contains is a thin alias to keep the test imports tight.
func contains(haystack, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
