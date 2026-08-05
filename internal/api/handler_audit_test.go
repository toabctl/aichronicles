package api

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/redact/redacttest"
)

// TestAuditSnippet_NeverEmitsRawSecret is the regression gate for the
// endpoint's core promise: "raw secret bytes never leave the server".
// The snippet builder used to concatenate the matched bytes into the
// buffer and swap in the marker as its last step, after normalising
// whitespace and applying the rune cap. Both rewrites desynchronised
// the needle from the buffer, so strings.Replace matched nothing and
// shipped the secret verbatim — precisely on the findings ingest-time
// redaction had missed, which is the output an operator pastes into a
// ticket.
func TestAuditSnippet_NeverEmitsRawSecret(t *testing.T) {
	t.Parallel()
	// Each secret is a real detector shape so redact.Default() finds
	// it; needle is the substring that must never survive.
	cases := []struct {
		name    string
		content string
		needle  string
	}{
		{
			name:    "long hit truncated past the rune cap",
			content: "some preceding context words here before the token: " + "sk-ant-api03-" + strings.Repeat("A", 95),
			needle:  "sk-ant-api03-AAAAAAAAAA",
		},
		{
			name:    "multi-line PEM flattened by the newline rewrite",
			content: "note: " + redacttest.PEMPrivateKey("MIIEowIBAAKCAQEAxGZ1qQb7cLP"),
			needle:  "MIIEowIBAAKCAQEAxGZ1qQb7cLP",
		},
		{
			name:    "hit at offset zero with no preceding context",
			content: "ghp_" + strings.Repeat("b", 36) + " trailing words",
			needle:  "ghp_bbbbbbbbbb",
		},
		{
			name:    "hit surrounded by multibyte runes",
			content: "→→→ context ünïcöde " + "AKIA" + strings.Repeat("C", 16) + " ←←← more ünïcöde",
			needle:  "AKIACCCCCCCCCCCCCCCC",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			findings := redact.Default().Scan(tc.content)
			if len(findings) == 0 {
				t.Fatalf("no finding for %q — test fixture no longer matches a detector", tc.content)
			}
			got := auditSnippet(tc.content, findings[0])
			if strings.Contains(got, tc.needle) {
				t.Errorf("snippet leaked raw secret bytes\n needle: %q\nsnippet: %q", tc.needle, got)
			}
			if marker := "<" + findings[0].Pattern + ">"; !strings.Contains(got, marker) {
				t.Errorf("snippet is missing the %q marker: %q", marker, got)
			}
			if n := utf8.RuneCountInString(got); n > auditSnippetRunes+1 {
				t.Errorf("snippet is %d runes, over the %d cap (+1 for the ellipsis): %q",
					n, auditSnippetRunes, got)
			}
			for _, ws := range []string{"\n", "\r", "\t"} {
				if strings.Contains(got, ws) {
					t.Errorf("snippet retained raw whitespace %q: %q", ws, got)
				}
			}
		})
	}
}

// TestAuditSnippet_ClampsOutOfRangeOffsets guards the arithmetic: a
// finding whose offsets don't address the content (a stale or
// hand-built Finding) must not panic the daemon.
func TestAuditSnippet_ClampsOutOfRangeOffsets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		start int
		end   int
	}{
		{"negative start", -5, 4},
		{"end past content", 0, 9999},
		{"start past end", 8, 2},
		{"both out of range", -1, 9999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := auditSnippet("short content", redact.Finding{
				Pattern: "test_pattern",
				Start:   tc.start,
				End:     tc.end,
			})
			if !strings.Contains(got, "<test_pattern>") {
				t.Errorf("expected marker in %q", got)
			}
		})
	}
}

// TestBuildAuditQuery_AlwaysIncludesLIMIT pins the ceiling: every
// call site (including the limit=0 default the handler now maps
// to auditMaxRowsCeiling) must produce a query with a LIMIT clause.
// Without the clamp the handler streamed every row in `events`
// through redact.Scanner — hundreds of MB of regex work on a real
// corpus, plus SQLite write-lock contention while the scan held.
func TestBuildAuditQuery_AlwaysIncludesLIMIT(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		since int64
		limit int
	}{
		{"no since, ceiling limit", 0, auditMaxRowsCeiling},
		{"with since, ceiling limit", 1_700_000_000_000, auditMaxRowsCeiling},
		{"small client-supplied limit", 0, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, args := buildAuditQuery(tc.since, tc.limit)
			if !strings.Contains(q, "LIMIT ?") {
				t.Errorf("query missing LIMIT clause:\n%s", q)
			}
			// Last bound argument must be the limit so SQLite sees
			// the clamp.
			if len(args) == 0 {
				t.Fatalf("args empty")
			}
			gotLimit, ok := args[len(args)-1].(int)
			if !ok {
				t.Fatalf("last arg should be int limit; got %T", args[len(args)-1])
			}
			if gotLimit != tc.limit {
				t.Errorf("LIMIT bound: got %d want %d", gotLimit, tc.limit)
			}
		})
	}
}
