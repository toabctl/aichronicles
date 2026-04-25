package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/ingest"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/redact"
)

// seedScrubStore plants two poisoned events + one clean event, all
// with Redaction.Applied=true so they pass the store invariant. This
// models "store was populated before the redactor shipped" — the
// exact thing scrub exists to fix.
func seedScrubStore(t *testing.T) *store.Store {
	t.Helper()
	s := testStore(t)
	now := time.Now().UTC()

	fixtures := []struct {
		kind    string
		content string
	}{
		{"user_prompt", "leak AKIAIOSFODNN7EXAMPLE in prompt"},
		{"assistant_message", "key sk-ant-" + strings.Repeat("x", 40) + " in reply"},
		{"user_prompt", "nothing to see here"},
	}
	for i, fx := range fixtures {
		env := ingest.Envelope{
			V:               1,
			EventID:         uuid.Must(uuid.NewV7()).String(),
			SourceAgent:     "claude-code",
			SourceSessionID: "sess-scrub",
			Kind:            fx.kind,
			Role:            "user",
			TsSource:        now.Add(time.Duration(i) * time.Second),
			ContentText:     fx.content,
			Payload:         map[string]any{"original": fx.content},
			Redaction:       &ingest.Redaction{Applied: true},
		}
		raw, _ := json.Marshal(env)
		tx, _ := s.DB().Begin()
		if _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			t.Fatalf("seed ingest: %v", err)
		}
		_ = tx.Commit()
	}
	return s
}

func TestRunScrub_DryRunReportsWithoutWriting(t *testing.T) {
	t.Parallel()
	s := seedScrubStore(t)

	var out bytes.Buffer
	report, err := RunScrub(s, redact.Default(), ScrubOptions{DryRun: true}, &out)
	if err != nil {
		t.Fatalf("RunScrub: %v", err)
	}
	if !report.DryRun {
		t.Error("report.DryRun must be true in dry-run mode")
	}
	if report.EventsScanned != 3 {
		t.Errorf("EventsScanned: got %d, want 3", report.EventsScanned)
	}
	if report.EnvelopesRewritten != 2 {
		t.Errorf("EnvelopesRewritten: got %d, want 2", report.EnvelopesRewritten)
	}

	// DB MUST be untouched.
	var poisoned int
	_ = s.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE content_text LIKE '%AKIA%' OR content_text LIKE '%sk-ant-%'`,
	).Scan(&poisoned)
	if poisoned != 2 {
		t.Errorf("dry-run mutated DB: poisoned rows now %d (want 2)", poisoned)
	}
	if !strings.Contains(out.String(), "would rewrite") {
		t.Errorf("expected 'would rewrite' in dry-run output: %s", out.String())
	}
}

func TestRunScrub_WriteModeRewritesBothLayers(t *testing.T) {
	t.Parallel()
	s := seedScrubStore(t)

	var out bytes.Buffer
	report, err := RunScrub(s, redact.Default(), ScrubOptions{DryRun: false}, &out)
	if err != nil {
		t.Fatalf("RunScrub: %v", err)
	}
	if report.DryRun {
		t.Error("report.DryRun must be false after write")
	}
	if report.EventsRewritten != 2 {
		t.Errorf("EventsRewritten: got %d, want 2", report.EventsRewritten)
	}

	// events.content_text — no raw secrets left.
	var poisoned int
	_ = s.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE content_text LIKE '%AKIA%' OR content_text LIKE '%sk-ant-%'`,
	).Scan(&poisoned)
	if poisoned != 0 {
		t.Errorf("events.content_text still poisoned: %d rows", poisoned)
	}

	// raw_envelopes.envelope_json — no raw secrets left.
	var rawPoisoned int
	_ = s.DB().QueryRow(
		`SELECT COUNT(*) FROM raw_envelopes WHERE envelope_json LIKE '%AKIA%' OR envelope_json LIKE '%sk-ant-%'`,
	).Scan(&rawPoisoned)
	if rawPoisoned != 0 {
		t.Errorf("raw_envelopes.envelope_json still poisoned: %d rows", rawPoisoned)
	}

	// Marker landed.
	var markers int
	_ = s.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE content_text LIKE '%<redacted:%'`,
	).Scan(&markers)
	if markers != 2 {
		t.Errorf("expected 2 rows with marker, got %d", markers)
	}

	// FTS stays consistent — searching for the old secret returns zero.
	var ftsLeak int
	_ = s.DB().QueryRow(
		`SELECT COUNT(*) FROM events_fts WHERE events_fts MATCH 'AKIAIOSFODNN7EXAMPLE'`,
	).Scan(&ftsLeak)
	if ftsLeak != 0 {
		t.Errorf("FTS index still points at the old secret: %d matches", ftsLeak)
	}
}

func TestRunScrub_IdempotentOnSecondRun(t *testing.T) {
	t.Parallel()
	s := seedScrubStore(t)

	var out bytes.Buffer
	if _, err := RunScrub(s, redact.Default(), ScrubOptions{DryRun: false}, &out); err != nil {
		t.Fatalf("first scrub: %v", err)
	}
	out.Reset()
	report, err := RunScrub(s, redact.Default(), ScrubOptions{DryRun: false}, &out)
	if err != nil {
		t.Fatalf("second scrub: %v", err)
	}
	if report.EventsRewritten != 0 {
		t.Errorf("second pass should be a no-op: rewrote %d", report.EventsRewritten)
	}
	if report.EnvelopesRewritten != 0 {
		t.Errorf("second pass should not touch raw: rewrote %d", report.EnvelopesRewritten)
	}
}

func TestRunScrub_CleanStorePrintsZeros(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var out bytes.Buffer
	report, err := RunScrub(s, redact.Default(), ScrubOptions{DryRun: true}, &out)
	if err != nil {
		t.Fatalf("RunScrub: %v", err)
	}
	if report.EventsScanned != 0 || report.EventsRewritten != 0 {
		t.Errorf("report: %+v", report)
	}
}
