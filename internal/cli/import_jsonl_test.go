package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/ingest"
	"github.com/toabctl/aichronicles/internal/store"
)

// testStore opens a fresh Store in a temp dir and wires teardown.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// jsonlFromEnvelopes renders one envelope per line, as aichroniclesd
// would have written to events.jsonl in the POC era.
func jsonlFromEnvelopes(t *testing.T, envs ...ingest.Envelope) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func newEnv(kind string) ingest.Envelope {
	return ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-import",
		Kind:            kind,
		Role:            "user",
		TsSource:        time.Now().UTC(),
		Cwd:             "/tmp/proj",
		ContentText:     "content for " + kind,
		Payload:         map[string]any{"kind": kind},
	}
}

func TestImportJSONL_HappyPath(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	data := jsonlFromEnvelopes(t,
		newEnv("user_prompt"),
		newEnv("assistant_message"),
		newEnv("tool_use"),
	)

	report, err := ImportJSONL(t.Context(), bytes.NewReader(data), s)
	if err != nil {
		t.Fatalf("ImportJSONL: %v", err)
	}
	if report.Imported != 3 || report.Deduped != 0 || report.Invalid != 0 {
		t.Errorf("report: %+v", report)
	}
	if report.LinesRead != 3 {
		t.Errorf("LinesRead: got %d, want 3", report.LinesRead)
	}

	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	if n != 3 {
		t.Errorf("events: got %d, want 3", n)
	}
}

func TestImportJSONL_IdempotentOnDuplicates(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	data := jsonlFromEnvelopes(t, newEnv("user_prompt"), newEnv("tool_use"))

	if _, err := ImportJSONL(t.Context(), bytes.NewReader(data), s); err != nil {
		t.Fatalf("first: %v", err)
	}
	report, err := ImportJSONL(t.Context(), bytes.NewReader(data), s)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if report.Imported != 0 {
		t.Errorf("expected 0 imported on replay, got %d", report.Imported)
	}
	if report.Deduped != 2 {
		t.Errorf("expected 2 deduped, got %d", report.Deduped)
	}

	// Raw count unchanged
	var raw int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM raw_envelopes`).Scan(&raw)
	if raw != 2 {
		t.Errorf("raw: got %d, want 2", raw)
	}
}

func TestImportJSONL_MalformedLinesAreCountedNotFatal(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	good1 := newEnv("user_prompt")
	good2 := newEnv("tool_use")
	goodB1, _ := json.Marshal(good1)
	goodB2, _ := json.Marshal(good2)

	var buf bytes.Buffer
	buf.Write(goodB1)
	buf.WriteByte('\n')
	buf.WriteString("{not json\n")
	buf.WriteString("\n") // blank line — skipped, not invalid
	// Valid JSON, but not a usable envelope (v=2 → validation fails)
	badEnv := good2
	badEnv.V = 2
	b, _ := json.Marshal(badEnv)
	buf.Write(b)
	buf.WriteByte('\n')
	buf.Write(goodB2)
	buf.WriteByte('\n')

	report, err := ImportJSONL(t.Context(), &buf, s)
	if err != nil {
		t.Fatalf("ImportJSONL: %v", err)
	}
	if report.Imported != 2 {
		t.Errorf("imported: got %d, want 2", report.Imported)
	}
	if report.Invalid != 2 {
		t.Errorf("invalid: got %d, want 2", report.Invalid)
	}
}

func TestImportJSONL_EmptyInput(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	report, err := ImportJSONL(t.Context(), bytes.NewReader(nil), s)
	if err != nil {
		t.Fatalf("ImportJSONL: %v", err)
	}
	if report.Imported != 0 || report.Deduped != 0 || report.Invalid != 0 {
		t.Errorf("report should be all-zero, got %+v", report)
	}
}

func TestImportJSONL_LargeContentSurvivesScannerBuffer(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	huge := strings.Repeat("lorem ipsum ", 100_000) // ~1.2 MB
	env := newEnv("assistant_message")
	env.ContentText = huge
	env.Payload = map[string]any{"text": huge}
	data := jsonlFromEnvelopes(t, env)

	report, err := ImportJSONL(t.Context(), bytes.NewReader(data), s)
	if err != nil {
		t.Fatalf("ImportJSONL: %v", err)
	}
	if report.Imported != 1 {
		t.Errorf("imported: got %d, want 1", report.Imported)
	}

	var content string
	_ = s.DB().QueryRow(`SELECT content_text FROM events`).Scan(&content)
	if len(content) != len(huge) {
		t.Errorf("round-trip size mismatch: got %d, want %d", len(content), len(huge))
	}
}

func TestImportJSONL_ScrubsSecretsEvenWhenInputClaimsApplied(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	secret := "sk-ant-" + strings.Repeat("a", 40)
	env := newEnv("user_prompt")
	env.ContentText = "here is my key " + secret
	// Input file LIES about being pre-scrubbed. The importer must not
	// trust the incoming Redaction.Applied and must scrub anyway.
	env.Redaction = &ingest.Redaction{Applied: true, Patterns: nil}
	data := jsonlFromEnvelopes(t, env)

	report, err := ImportJSONL(t.Context(), bytes.NewReader(data), s)
	if err != nil {
		t.Fatalf("ImportJSONL: %v", err)
	}
	if report.Imported != 1 {
		t.Fatalf("expected 1 import, got %+v", report)
	}

	var content, raw string
	_ = s.DB().QueryRow(`SELECT content_text FROM events`).Scan(&content)
	_ = s.DB().QueryRow(`SELECT envelope_json FROM raw_envelopes`).Scan(&raw)
	if strings.Contains(content, "sk-ant-") {
		t.Errorf("events.content_text still has secret: %q", content)
	}
	if strings.Contains(raw, "sk-ant-") {
		t.Errorf("raw_envelopes.envelope_json still has secret: %q", raw)
	}
	if !strings.Contains(content, "<redacted:anthropic_api_key>") {
		t.Errorf("expected marker in content: %q", content)
	}
}

func TestImportJSONL_ReportStringMentionsAllFields(t *testing.T) {
	t.Parallel()
	r := ImportReport{LinesRead: 4, Imported: 2, Deduped: 1, Invalid: 1, DurationMS: 7}
	s := r.String()
	for _, want := range []string{"lines read:   4", "imported:     2", "deduped:      1", "invalid:      1", "duration_ms:  7"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in report:\n%s", want, s)
		}
	}
}
