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

	// Header + 2 rows + totals line = 4 lines.
	body := out.String()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected header + 2 rows + totals = 4 lines, got %d:\n%s", len(lines), body)
	}
	if want := "scanned 4 events, 2 flagged (2 findings)"; lines[3] != want {
		t.Errorf("totals line: got %q, want %q", lines[3], want)
	}
	// Snippet must NEVER contain the raw secret — that's the whole
	// point of audit: produce safely-copyable output.
	for _, l := range lines[1:3] {
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
	// Limit=1 caps rows scanned, not Flagged. Rows are scanned most-
	// recently-ingested first, so limit=1 sees only the last fixture.
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
	if got, want := out.String(), "(no findings — scanned 0 events, 0 flagged (0 findings))\n"; got != want {
		t.Errorf("empty-state line: got %q, want %q", got, want)
	}
}

// TestRunAudit_FollowsCursorAcrossPages is the regression gate for the
// "0 = scan all" promise: the server bounds every page, so a CLI that
// read only the first page reported a clean audit over a fraction of
// the store. A multi-page scan must equal the single-page one.
func TestRunAudit_FollowsCursorAcrossPages(t *testing.T) {
	t.Parallel()
	s := seedAuditStore(t)
	c := apiForStore(t, s)

	run := func(opts AuditOptions) AuditReportJSON {
		t.Helper()
		opts.Format = FormatJSON
		var out bytes.Buffer
		if err := runAudit(t.Context(), c, opts, &out); err != nil {
			t.Fatalf("runAudit %+v: %v", opts, err)
		}
		var got AuditReportJSON
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("json: %v", err)
		}
		return got
	}
	whole := run(AuditOptions{})
	for _, pageSize := range []int{1, 2, 3, 4} {
		got := run(AuditOptions{pageSize: pageSize})
		if got.Scanned != whole.Scanned || got.Flagged != whole.Flagged ||
			got.TotalFindings != whole.TotalFindings || len(got.Findings) != len(whole.Findings) {
			t.Errorf("pageSize=%d: got scanned=%d flagged=%d findings=%d, want %d/%d/%d",
				pageSize, got.Scanned, got.Flagged, len(got.Findings),
				whole.Scanned, whole.Flagged, len(whole.Findings))
		}
		for name, n := range whole.PatternHits {
			if got.PatternHits[name] != n {
				t.Errorf("pageSize=%d: pattern %s hits %d, want %d", pageSize, name, got.PatternHits[name], n)
			}
		}
		for i := range got.Findings {
			if got.Findings[i].Snippet != whole.Findings[i].Snippet {
				t.Errorf("pageSize=%d: finding %d out of order", pageSize, i)
			}
		}
	}
	if whole.Scanned != 4 {
		t.Fatalf("baseline scanned %d, want 4", whole.Scanned)
	}
}

// TestRunAudit_LimitSpansPages pins that --limit counts rows across
// pages: it bounds the total scanned, never a single page.
func TestRunAudit_LimitSpansPages(t *testing.T) {
	t.Parallel()
	s := seedAuditStore(t)
	c := apiForStore(t, s)
	for _, tc := range []struct{ limit, pageSize, want int }{
		{3, 2, 3},
		{3, 1, 3},
		{2, 2, 2},
		{10, 3, 4},
	} {
		var out bytes.Buffer
		if err := runAudit(t.Context(), c, AuditOptions{Limit: tc.limit, pageSize: tc.pageSize, Format: FormatJSON}, &out); err != nil {
			t.Fatalf("runAudit: %v", err)
		}
		var got AuditReportJSON
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("json: %v", err)
		}
		if got.Scanned != tc.want {
			t.Errorf("limit=%d pageSize=%d: scanned %d, want %d", tc.limit, tc.pageSize, got.Scanned, tc.want)
		}
	}
}
