package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
)

// seedAuditStore writes two benign events and two events containing
// synthetic secrets directly to the DB layer via store.IngestEnvelope.
// IMPORTANT: we intentionally bypass the normal redaction flow so the
// audit command has secrets to find — the scenario being modelled is
// "store was populated before the redactor existed". To dodge the
// store-level invariant we set Applied=true on the envelope but leave
// the secret in place, same as a lying client would.
func seedAuditStore(t *testing.T) *store.Store {
	t.Helper()
	s := testStore(t)
	now := time.Now().UTC()

	fixtures := []struct {
		kind    string
		content string
	}{
		{"user_prompt", "benign prompt about jsonl"},
		{"assistant_message", "here is a bare AKIAIOSFODNN7EXAMPLE key"},
		{"user_prompt", "my token sk-ant-" + strings.Repeat("a", 40) + " oops"},
		{"assistant_message", "another benign line"},
	}
	for i, fx := range fixtures {
		env := events.Envelope{
			V:               1,
			EventID:         uuid.Must(uuid.NewV7()).String(),
			SourceAgent:     "claude-code",
			SourceSessionID: "sess-audit",
			Kind:            fx.kind,
			Role:            "user",
			TsSource:        now.Add(time.Duration(i) * time.Second),
			ContentText:     fx.content,
			Payload:         map[string]any{"i": i},
			Redaction:       &events.Redaction{Applied: true},
		}
		raw, _ := json.Marshal(env)
		tx, err := s.DB().Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			t.Fatalf("seed ingest: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	return s
}

func TestRunAudit_FindsSeededSecrets(t *testing.T) {
	t.Parallel()
	s := seedAuditStore(t)
	c := apiForStore(t, s)

	var out bytes.Buffer
	if err := runAudit(t.Context(), c, AuditOptions{}, &out); err != nil {
		t.Fatalf("runAudit: %v", err)
	}

	// Header + 2 rows = 3 lines.
	body := out.String()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header + 2 output lines = 3, got %d:\n%s", len(lines), body)
	}
	// Snippet must NEVER contain the raw secret — that's the whole
	// point of audit: produce safely-copyable output.
	for _, l := range lines[1:] {
		if strings.Contains(l, "AKIAIOSFODNN7EXAMPLE") {
			t.Errorf("audit row leaked raw aws key: %q", l)
		}
		if strings.Contains(l, "sk-ant-a") {
			t.Errorf("audit row leaked raw anthropic key: %q", l)
		}
	}
	// Both expected pattern markers should be present in the output.
	for _, want := range []string{"<aws_access_key>", "<anthropic_api_key>"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing marker %q in audit output:\n%s", want, body)
		}
	}
}

func TestRunAudit_JSONReportsAggregates(t *testing.T) {
	t.Parallel()
	s := seedAuditStore(t)
	c := apiForStore(t, s)

	var out bytes.Buffer
	if err := runAudit(t.Context(), c, AuditOptions{Format: FormatJSON}, &out); err != nil {
		t.Fatalf("runAudit: %v", err)
	}
	var got AuditReportJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if got.Scanned != 4 {
		t.Errorf("Scanned: got %d, want 4", got.Scanned)
	}
	if got.Flagged != 2 {
		t.Errorf("Flagged: got %d, want 2", got.Flagged)
	}
	if got.PatternHits["aws_access_key"] != 1 {
		t.Errorf("aws hits: got %d, want 1", got.PatternHits["aws_access_key"])
	}
	if got.PatternHits["anthropic_api_key"] != 1 {
		t.Errorf("anthropic hits: got %d, want 1", got.PatternHits["anthropic_api_key"])
	}
}

func TestRunAudit_RespectsLimit(t *testing.T) {
	t.Parallel()
	s := seedAuditStore(t)
	c := apiForStore(t, s)

	var out bytes.Buffer
	// Limit=1 caps rows fetched from SQLite, not Flagged. With 4 rows
	// ordered by ts DESC, limit=1 returns the newest (benign), so
	// nothing flags.
	if err := runAudit(t.Context(), c, AuditOptions{Limit: 1, Format: FormatJSON}, &out); err != nil {
		t.Fatalf("runAudit: %v", err)
	}
	var got AuditReportJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got.Scanned != 1 {
		t.Errorf("Scanned should equal Limit: got %d", got.Scanned)
	}
}

func TestRunAudit_RespectsSinceFilter(t *testing.T) {
	t.Parallel()
	s := seedAuditStore(t)
	c := apiForStore(t, s)

	var out bytes.Buffer
	// Since 10 minutes ago — all 4 fixture events are within the last
	// 4 seconds, so nothing is excluded.
	if err := runAudit(t.Context(), c,
		AuditOptions{SinceMs: time.Now().Add(-10 * time.Minute).UnixMilli(), Format: FormatJSON},
		&out); err != nil {
		t.Fatalf("runAudit: %v", err)
	}
	var got AuditReportJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got.Scanned != 4 {
		t.Errorf("Scanned: got %d, want 4", got.Scanned)
	}

	// Same call with Since in the future — should exclude everything.
	out.Reset()
	if err := runAudit(t.Context(), c,
		AuditOptions{SinceMs: time.Now().Add(10 * time.Minute).UnixMilli(), Format: FormatJSON},
		&out); err != nil {
		t.Fatalf("runAudit future: %v", err)
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got.Scanned != 0 {
		t.Errorf("future Since: Scanned got %d, want 0", got.Scanned)
	}
}

func TestRunAudit_EmptyStoreShowsEmptyStateLine(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	c := apiForStore(t, s)
	var out bytes.Buffer
	if err := runAudit(t.Context(), c, AuditOptions{}, &out); err != nil {
		t.Fatalf("runAudit: %v", err)
	}
	if !strings.Contains(out.String(), "(no findings)") {
		t.Errorf("expected empty-state line, got %q", out.String())
	}
}
