package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/wire"
)

// ingestForTest POSTs env through /v1/ingest so the real extractors
// populate the extractions table.
func ingestForTest(t *testing.T, srv *testServer, env events.Envelope) {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env))))
	if rr.Code != http.StatusOK {
		t.Fatalf("ingest: %d %s", rr.Code, rr.Body.String())
	}
}

// prCreatedEnvelope is a Bash tool_use whose result carries Claude
// Code's gitOperation record for a newly created PR, with the url on
// its own stdout line — what PRCreatedExtractor turns into pr_created.
func prCreatedEnvelope(t *testing.T, sessionKey, cwd, prURL string, ts time.Time) events.Envelope {
	t.Helper()
	env := validEnvelope(t)
	env.SourceSessionID = sessionKey
	env.Kind = "tool_use"
	env.Cwd = cwd
	env.TsSource = ts
	env.Tool = &events.Tool{Name: "Bash"}
	env.ContentText = "Bash gh pr create --fill"
	env.Payload = map[string]any{
		"hook_event_name": "PostToolUse",
		"tool_input":      map[string]any{"command": "gh pr create --fill"},
		"tool_response": map[string]any{
			"stdout": prURL + "\n",
			"gitOperation": map[string]any{
				"pr": map[string]any{"action": "created", "number": 1, "url": prURL},
			},
		},
	}
	return env
}

// mentionEnvelope is a user prompt that merely mentions url, which the
// URL extractor records as kind=url.
func mentionEnvelope(t *testing.T, sessionKey, cwd, mentioned string, ts time.Time) events.Envelope {
	t.Helper()
	env := validEnvelope(t)
	env.SourceSessionID = sessionKey
	env.Cwd = cwd
	env.TsSource = ts
	env.ContentText = "please review " + mentioned
	return env
}

func getExtractions(t *testing.T, srv *testServer, query string) (int, wire.ExtractionsResponse, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/extractions?"+query, nil))
	var out wire.ExtractionsResponse
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rr.Body.String())
		}
	}
	return rr.Code, out, rr.Body.String()
}

func TestHandleExtractions_RejectsBadParams(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for _, q := range []string{
		"",                            // kind missing
		"value=https://g/o/r/pull/1",  // kind missing, value alone isn't enough
		"kind=",                       // kind empty
		"kind=pr_created&since_ms=-1", // negative window
		"kind=pr_created&since_ms=abc",
		"kind=pr_created&limit=0",
		"kind=pr_created&limit=-3",
		"kind=pr_created&limit=abc",
		"kind=pr_created&cursor=not-a-cursor",
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			if code, _, body := getExtractions(t, srv, q); code != http.StatusBadRequest {
				t.Errorf("status=%d body=%s, want 400", code, body)
			}
		})
	}
}

func TestHandleExtractions_EmptyResultIsEmptyArray(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	code, out, body := getExtractions(t, srv, "kind=pr_created&value="+url.QueryEscape("https://g/o/r/pull/1"))
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if !contains(body, `"extractions":[]`) {
		t.Errorf("want an empty array, not null: %s", body)
	}
	if out.NextCursor != "" {
		t.Errorf("empty result must not carry a next_cursor, got %q", out.NextCursor)
	}
}

// TestHandleExtractions_LookupSeparatesCreatorFromMentions drives the
// real ingest path: one session creates the PR, another only mentions
// it, a third creates a different PR. kind=pr_created&value= must name
// just the creator (with its cwd and timestamp); kind=url just the
// mentioner.
func TestHandleExtractions_LookupSeparatesCreatorFromMentions(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	const pr = "https://github.com/acme/widgets/pull/7"
	created := time.UnixMilli(1_700_000_000_000).UTC()
	mentioned := created.Add(time.Hour)
	ingestForTest(t, srv, prCreatedEnvelope(t, "creator", "/work/creator", pr, created))
	ingestForTest(t, srv, mentionEnvelope(t, "reviewer", "/work/reviewer", pr, mentioned))
	// A different PR, created elsewhere: the value filter must exclude it.
	ingestForTest(t, srv, prCreatedEnvelope(t, "other", "/work/other", "https://github.com/acme/widgets/pull/8", mentioned))
	creatorID := events.DeriveSessionID("claude-code", "creator")
	reviewerID := events.DeriveSessionID("claude-code", "reviewer")

	cwd := "/work/creator"
	code, out, body := getExtractions(t, srv, "kind=pr_created&value="+url.QueryEscape(pr))
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	want := []wire.ExtractionSighting{{
		SessionID: creatorID, Kind: events.ExtractionKindPRCreated, Value: pr,
		TsSourceMs: created.UnixMilli(), Cwd: &cwd,
	}}
	if !reflect.DeepEqual(out.Extractions, want) {
		t.Errorf("pr_created: got %+v, want %+v", out.Extractions, want)
	}

	code, out, body = getExtractions(t, srv, "kind=url&value="+url.QueryEscape(pr))
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if len(out.Extractions) != 1 || out.Extractions[0].SessionID != reviewerID {
		t.Errorf("url: got %+v, want only the reviewer session %s", out.Extractions, reviewerID)
	}
}

// TestHandleExtractions_PaginatesWithCursor lists pr_created in pages
// of two: pages carry a next_cursor until the short last page, are
// disjoint, and add up to the full listing.
func TestHandleExtractions_PaginatesWithCursor(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	base := time.UnixMilli(1_700_000_000_000).UTC()
	for i := range 5 {
		ingestForTest(t, srv, prCreatedEnvelope(t, fmt.Sprintf("s%d", i), "/w",
			fmt.Sprintf("https://github.com/acme/widgets/pull/%d", i), base.Add(time.Duration(i)*time.Minute)))
	}

	var seen []string
	query := "kind=pr_created&limit=2"
	for page := 0; ; page++ {
		if page > 5 {
			t.Fatal("pagination did not terminate")
		}
		code, out, body := getExtractions(t, srv, query)
		if code != http.StatusOK {
			t.Fatalf("page %d: status=%d body=%s", page, code, body)
		}
		for _, x := range out.Extractions {
			seen = append(seen, x.Value)
		}
		if out.NextCursor == "" {
			if len(out.Extractions) >= 2 {
				t.Errorf("page %d: full page without a next_cursor", page)
			}
			break
		}
		query = "kind=pr_created&limit=2&cursor=" + url.QueryEscape(string(out.NextCursor))
	}
	want := []string{
		"https://github.com/acme/widgets/pull/4",
		"https://github.com/acme/widgets/pull/3",
		"https://github.com/acme/widgets/pull/2",
		"https://github.com/acme/widgets/pull/1",
		"https://github.com/acme/widgets/pull/0",
	}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("paged values %v, want %v", seen, want)
	}
}

func TestHandleExtractions_SinceMsFilters(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	old := time.UnixMilli(1_700_000_000_000).UTC()
	recent := old.Add(24 * time.Hour)
	ingestForTest(t, srv, prCreatedEnvelope(t, "old", "/w", "https://github.com/acme/widgets/pull/1", old))
	ingestForTest(t, srv, prCreatedEnvelope(t, "recent", "/w", "https://github.com/acme/widgets/pull/2", recent))

	code, out, body := getExtractions(t, srv, fmt.Sprintf("kind=pr_created&since_ms=%d", recent.UnixMilli()))
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if len(out.Extractions) != 1 || out.Extractions[0].Value != "https://github.com/acme/widgets/pull/2" {
		t.Errorf("got %+v, want only the recent PR", out.Extractions)
	}
}

// TestHandleExtractions_OversizedLimitIsClamped follows the shared
// page-size contract: asking for more than MaxPageLimit is not an
// error.
func TestHandleExtractions_OversizedLimitIsClamped(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	if code, _, body := getExtractions(t, srv, fmt.Sprintf("kind=pr_created&limit=%d", wire.MaxPageLimit+1)); code != http.StatusOK {
		t.Errorf("status=%d body=%s, want 200", code, body)
	}
}
