package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/redact/redacttest"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
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
			got := auditSnippet(tc.content, findings)
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

// TestAuditSnippet_MasksEverySecretInWindow pins the multi-secret
// case: the window is cut around the first finding, so any later
// finding within ±padding runes used to ship verbatim in the context.
// Every finding in the row must appear only in marker form.
func TestAuditSnippet_MasksEverySecretInWindow(t *testing.T) {
	t.Parallel()
	ghp := "ghp_" + strings.Repeat("b", 36)
	aws := "AKIA" + strings.Repeat("C", 16)
	cases := []struct {
		name    string
		content string
		needles []string
	}{
		{
			name:    "second secret right after the first",
			content: "token " + ghp + " and key " + aws + " end",
			needles: []string{"ghp_bbbbbbbbbb", "AKIACCCCCCCCCCCC"},
		},
		{
			name:    "three secrets with the first pattern repeated",
			content: aws + " x " + ghp + " y " + aws,
			needles: []string{"ghp_bbbbbbbbbb", "AKIACCCCCCCCCCCC"},
		},
		{
			name:    "adjacent secrets with no separator text",
			content: ghp + " " + aws,
			needles: []string{"ghp_bbbbbbbbbb", "AKIACCCCCCCCCCCC"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			findings := redact.Default().Scan(tc.content)
			if len(findings) < 2 {
				t.Fatalf("got %d findings for %q, want >=2 — fixture no longer matches the detectors", len(findings), tc.content)
			}
			got := auditSnippet(tc.content, findings)
			for _, n := range tc.needles {
				if strings.Contains(got, n) {
					t.Errorf("snippet leaked raw secret bytes\n needle: %q\nsnippet: %q", n, got)
				}
			}
			for _, f := range findings {
				if marker := "<" + f.Pattern + ">"; !strings.Contains(got, marker) {
					t.Errorf("snippet is missing the %q marker: %q", marker, got)
				}
			}
		})
	}
}

// TestAuditSnippet_UnsortedAndOverlappingFindings guards the
// defensive path: findings handed over out of order, or overlapping,
// must still mask every byte any finding covers.
func TestAuditSnippet_UnsortedAndOverlappingFindings(t *testing.T) {
	t.Parallel()
	content := "aaa SECRETONE bbb SECRETTWO ccc"
	one := strings.Index(content, "SECRETONE")
	two := strings.Index(content, "SECRETTWO")
	cases := []struct {
		name     string
		findings []redact.Finding
	}{
		{"reverse order", []redact.Finding{
			{Pattern: "p2", Start: two, End: two + len("SECRETTWO")},
			{Pattern: "p1", Start: one, End: one + len("SECRETONE")},
		}},
		{"overlap extends past the first span", []redact.Finding{
			{Pattern: "p1", Start: one, End: one + 3},
			{Pattern: "p2", Start: one + 1, End: one + len("SECRETONE")},
			{Pattern: "p3", Start: two, End: two + len("SECRETTWO")},
		}},
		{"nested overlap", []redact.Finding{
			{Pattern: "p1", Start: one, End: two + len("SECRETTWO")},
			{Pattern: "p2", Start: two, End: two + 3},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := auditSnippet(content, tc.findings)
			for _, frag := range []string{"SECRET", "ONE", "TWO"} {
				if strings.Contains(got, frag) {
					t.Errorf("snippet leaked %q: %q", frag, got)
				}
			}
			if !strings.HasPrefix(got, "aaa <") || !strings.HasSuffix(got, " ccc") {
				t.Errorf("surrounding context lost: %q", got)
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
			got := auditSnippet("short content", []redact.Finding{{
				Pattern: "test_pattern",
				Start:   tc.start,
				End:     tc.end,
			}})
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
		name   string
		since  int64
		before int64
		limit  int
	}{
		{"no since, ceiling limit", 0, 0, auditMaxRowsCeiling},
		{"with since, ceiling limit", 1_700_000_000_000, 0, auditMaxRowsCeiling},
		{"small client-supplied limit", 0, 0, 10},
		{"keyset page with since", 1_700_000_000_000, 42, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, args := buildAuditQuery(tc.since, tc.before, tc.limit)
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

// seedAuditEvents writes one event per content string straight
// through store.IngestEnvelope, bypassing the ingest redactor so the
// audit has secrets to find (the "stored before the detector existed"
// scenario). tsOffsets[i] is added to a fixed base for event i's
// ts_source, so a test can decouple source-time order from ingest
// order. Returns the ts_source_ms of each event.
func seedAuditEvents(t *testing.T, srv *testServer, contents []string, tsOffsets []time.Duration) []int64 {
	t.Helper()
	base := time.UnixMilli(1_750_000_000_000).UTC()
	out := make([]int64, len(contents))
	for i, c := range contents {
		ts := base.Add(tsOffsets[i])
		env := events.Envelope{
			V:               1,
			EventID:         uuid.Must(uuid.NewV7()).String(),
			SourceAgent:     "claude-code",
			SourceSessionID: "sess-audit",
			Kind:            "user_prompt",
			TsSource:        ts,
			ContentText:     c,
			Payload:         map[string]any{"i": i},
			Redaction:       &events.Redaction{Applied: true},
		}
		raw := mustJSON(t, env)
		tx, err := srv.store.DB().Begin()
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
		out[i] = ts.UnixMilli()
	}
	return out
}

func getAudit(t *testing.T, srv *testServer, query string) (int, wire.AuditResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit?"+query, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	var resp wire.AuditResponse
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v\n%s", err, rr.Body.String())
		}
	}
	return rr.Code, resp
}

// TestHandleAudit_KeysetPagesCoverEveryRowOnce is the regression gate
// for the scan cap: the endpoint used to return the newest 5000 rows
// with no signal that more existed, so a "scan all" audit silently
// covered a fraction of the store. Paging must visit every row exactly
// once, in ingest order, independent of ts_source order, and stop on
// an empty next_cursor.
func TestHandleAudit_KeysetPagesCoverEveryRowOnce(t *testing.T) {
	t.Parallel()
	ghp := "ghp_" + strings.Repeat("b", 36)
	cases := []struct {
		name  string
		rows  int
		limit int
		pages int // requests until next_cursor comes back empty
	}{
		{"uneven last page", 5, 2, 3},
		{"exact multiple needs one empty page", 4, 2, 3},
		{"single short page", 3, 10, 1},
		{"page size one", 3, 1, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newTestServer(t)
			contents := make([]string, tc.rows)
			offsets := make([]time.Duration, tc.rows)
			for i := range contents {
				// Every row flags so each is identifiable in Findings;
				// ts_source runs backwards so a ts-ordered scan would
				// visit rows in the opposite order to ingest.
				contents[i] = "row " + strconv.Itoa(i) + " " + ghp
				offsets[i] = -time.Duration(i) * time.Minute
			}
			ts := seedAuditEvents(t, srv, contents, offsets)

			var (
				seen   []int64
				cursor string
				pages  int
			)
			for {
				q := "limit=" + strconv.Itoa(tc.limit)
				if cursor != "" {
					q += "&cursor=" + cursor
				}
				code, resp := getAudit(t, srv, q)
				if code != http.StatusOK {
					t.Fatalf("page %d: status %d", pages, code)
				}
				pages++
				if resp.Scanned != len(resp.Findings) {
					t.Fatalf("page %d: scanned %d but %d findings; every seeded row flags", pages, resp.Scanned, len(resp.Findings))
				}
				for _, f := range resp.Findings {
					seen = append(seen, *f.TsSourceMs)
				}
				if resp.NextCursor == "" {
					break
				}
				cursor = string(resp.NextCursor)
				if pages > tc.rows+1 {
					t.Fatalf("pagination did not terminate after %d pages", pages)
				}
			}
			if pages != tc.pages {
				t.Errorf("pages: got %d, want %d", pages, tc.pages)
			}
			// Most-recently-ingested first: the reverse of seed order.
			want := make([]int64, 0, len(ts))
			for i := len(ts) - 1; i >= 0; i-- {
				want = append(want, ts[i])
			}
			if !slices.Equal(seen, want) {
				t.Errorf("visited rows (ts_source_ms) in wrong order or not exactly once:\n got %v\nwant %v", seen, want)
			}
		})
	}
}

// TestHandleAudit_SinceFilterAppliesOnEveryPage pins that since_ms is
// re-applied per page rather than only to the first.
func TestHandleAudit_SinceFilterAppliesOnEveryPage(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	ghp := "ghp_" + strings.Repeat("b", 36)
	// Alternate old/new source times across ingest order.
	offsets := []time.Duration{0, -48 * time.Hour, time.Minute, -49 * time.Hour, 2 * time.Minute}
	contents := make([]string, len(offsets))
	for i := range contents {
		contents[i] = "row " + strconv.Itoa(i) + " " + ghp
	}
	ts := seedAuditEvents(t, srv, contents, offsets)
	since := ts[0] - int64(time.Hour/time.Millisecond)

	total := 0
	cursor := ""
	for range 10 {
		q := "limit=1&since_ms=" + strconv.FormatInt(since, 10)
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		code, resp := getAudit(t, srv, q)
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		for _, f := range resp.Findings {
			if *f.TsSourceMs < since {
				t.Errorf("row with ts %d older than since_ms %d leaked into a page", *f.TsSourceMs, since)
			}
		}
		total += resp.Scanned
		if resp.NextCursor == "" {
			break
		}
		cursor = string(resp.NextCursor)
	}
	if total != 3 {
		t.Errorf("scanned %d rows across pages, want the 3 inside the window", total)
	}
}

func TestHandleAudit_RejectsBadCursor(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	zero, err := wire.EncodeAuditCursor(wire.AuditCursor{BeforeSeq: 0})
	if err != nil {
		t.Fatal(err)
	}
	neg, err := wire.EncodeAuditCursor(wire.AuditCursor{BeforeSeq: -3})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]string{
		"not base64":   "!!!",
		"zero seq":     string(zero),
		"negative seq": string(neg),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if code, _ := getAudit(t, srv, "cursor="+c); code != http.StatusBadRequest {
				t.Errorf("status: got %d, want 400", code)
			}
		})
	}
}
