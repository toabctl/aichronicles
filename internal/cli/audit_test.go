package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/redact"
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
		env := ingest.Envelope{
			V:               1,
			EventID:         uuid.Must(uuid.NewV7()).String(),
			SourceAgent:     "claude-code",
			SourceSessionID: "sess-audit",
			Kind:            fx.kind,
			Role:            "user",
			TsSource:        now.Add(time.Duration(i) * time.Second),
			ContentText:     fx.content,
			Payload:         map[string]any{"i": i},
			Redaction:       &ingest.Redaction{Applied: true},
		}
		raw, _ := json.Marshal(env)
		tx, err := s.DB().Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
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

	var out bytes.Buffer
	report, err := RunAudit(s, redact.Default(), AuditOptions{}, &out)
	if err != nil {
		t.Fatalf("RunAudit: %v", err)
	}

	if report.Scanned != 4 {
		t.Errorf("Scanned: got %d, want 4", report.Scanned)
	}
	if report.Flagged != 2 {
		t.Errorf("Flagged: got %d, want 2", report.Flagged)
	}
	if report.TotalFindings != 2 {
		t.Errorf("TotalFindings: got %d, want 2", report.TotalFindings)
	}
	if report.PatternHits["aws_access_key"] != 1 {
		t.Errorf("aws hits: got %d, want 1", report.PatternHits["aws_access_key"])
	}
	if report.PatternHits["anthropic_api_key"] != 1 {
		t.Errorf("anthropic hits: got %d, want 1", report.PatternHits["anthropic_api_key"])
	}

	// Two rows written.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 output lines, got %d:\n%s", len(lines), out.String())
	}
	// Snippet must NEVER contain the raw secret — that's the whole
	// point of audit: produce safely-copyable output.
	for _, l := range lines {
		if strings.Contains(l, "AKIAIOSFODNN7EXAMPLE") {
			t.Errorf("audit row leaked raw aws key: %q", l)
		}
		if strings.Contains(l, "sk-ant-a") {
			t.Errorf("audit row leaked raw anthropic key: %q", l)
		}
	}
}

func TestRunAudit_RespectsLimit(t *testing.T) {
	t.Parallel()
	s := seedAuditStore(t)

	var out bytes.Buffer
	report, err := RunAudit(s, redact.Default(), AuditOptions{Limit: 1}, &out)
	if err != nil {
		t.Fatalf("RunAudit: %v", err)
	}
	// Limit caps rows fetched from SQLite, not Flagged. With 4 rows
	// ordered by ts DESC, limit=1 returns the newest (benign), so
	// nothing flags.
	if report.Scanned != 1 {
		t.Errorf("Scanned should equal Limit: got %d", report.Scanned)
	}
}

func TestRunAudit_RespectsSinceFilter(t *testing.T) {
	t.Parallel()
	s := seedAuditStore(t)

	// Set Since to 10 minutes ago — all 4 fixture events are within
	// the last 4 seconds, so nothing is excluded.
	var out bytes.Buffer
	report, err := RunAudit(s, redact.Default(),
		AuditOptions{SinceMs: time.Now().Add(-10 * time.Minute).UnixMilli()}, &out)
	if err != nil {
		t.Fatalf("RunAudit: %v", err)
	}
	if report.Scanned != 4 {
		t.Errorf("Scanned: got %d, want 4", report.Scanned)
	}

	// Same call with Since in the future — should exclude everything.
	out.Reset()
	report, err = RunAudit(s, redact.Default(),
		AuditOptions{SinceMs: time.Now().Add(10 * time.Minute).UnixMilli()}, &out)
	if err != nil {
		t.Fatalf("RunAudit future: %v", err)
	}
	if report.Scanned != 0 {
		t.Errorf("future Since: Scanned got %d, want 0", report.Scanned)
	}
}

func TestRunAudit_EmptyStoreNoRowsNoError(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var out bytes.Buffer
	report, err := RunAudit(s, redact.Default(), AuditOptions{}, &out)
	if err != nil {
		t.Fatalf("RunAudit: %v", err)
	}
	if report.Scanned != 0 || report.Flagged != 0 {
		t.Errorf("empty store: %+v", report)
	}
	if out.Len() != 0 {
		t.Errorf("empty store should produce no output, got %q", out.String())
	}
}

func TestAuditSnippet_ReplacesSecretWithMarker(t *testing.T) {
	t.Parallel()
	secret := "AKIAIOSFODNN7EXAMPLE"
	content := "blah blah " + secret + " trailing"
	findings := redact.Default().Scan(content)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	s := auditSnippet(content, findings[0])
	if strings.Contains(s, secret) {
		t.Errorf("snippet leaked secret: %q", s)
	}
	if !strings.Contains(s, "<aws_access_key>") {
		t.Errorf("expected marker in snippet: %q", s)
	}
}
