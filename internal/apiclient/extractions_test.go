package apiclient

import (
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/wire"
)

// TestClient_Extractions_EncodesQuery pins the query string: every
// set field is sent, a value with query-significant characters
// survives encoding exactly, and zero-value fields are omitted.
func TestClient_Extractions_EncodesQuery(t *testing.T) {
	t.Parallel()
	const tricky = "https://g/o/r/pull/1?a=1&b=two words+x#frag/ä"
	tests := []struct {
		name string
		req  wire.ExtractionsRequest
		want url.Values
	}{
		{
			name: "all fields",
			req: wire.ExtractionsRequest{
				Kind: "pr_created", Value: tricky, SinceMs: 1234, Limit: 7, Cursor: "abc",
			},
			want: url.Values{
				"kind":     {"pr_created"},
				"value":    {tricky},
				"since_ms": {"1234"},
				"limit":    {"7"},
				"cursor":   {"abc"},
			},
		},
		{
			name: "zero values omitted",
			req:  wire.ExtractionsRequest{Kind: "pr_created"},
			want: url.Values{"kind": {"pr_created"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got url.Values
			var path string
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, path = r.URL.Query(), r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"extractions":[]}`))
			}))
			if _, err := c.Extractions(t.Context(), tt.req); err != nil {
				t.Fatalf("Extractions: %v", err)
			}
			if path != "/v1/extractions" {
				t.Errorf("path: got %q, want /v1/extractions", path)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("query: got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestClient_Extractions_RealServer drives ingest → extractor → store
// → handler → client: a created PR is found by exact value with its
// session, timestamp and cwd, and a URL full of query-significant
// characters round-trips as a lookup key.
func TestClient_Extractions_RealServer(t *testing.T) {
	t.Parallel()
	c, st := newRealServerClient(t)

	const pr = "https://github.com/acme/widgets/pull/7"
	created := time.UnixMilli(1_700_000_000_000).UTC()
	env := validEnvelope(t)
	env.SourceSessionID = "creator"
	env.Kind = "tool_use"
	env.Cwd = "/work/creator"
	env.TsSource = created
	env.Tool = &events.Tool{Name: "Bash"}
	env.Payload = map[string]any{
		"tool_input": map[string]any{"command": "gh pr create --fill"},
		"tool_response": map[string]any{
			"stdout": pr + "\n",
			"gitOperation": map[string]any{
				"pr": map[string]any{"action": "created", "number": 7, "url": pr},
			},
		},
	}
	if _, err := c.Ingest(t.Context(), env); err != nil {
		t.Fatalf("ingest PR: %v", err)
	}

	const tricky = "https://example.com/pull/1?a=1&b=2#frag"
	mention := validEnvelope(t)
	mention.SourceSessionID = "mentioner"
	mention.ContentText = "see " + tricky + " please"
	if _, err := c.Ingest(t.Context(), mention); err != nil {
		t.Fatalf("ingest mention: %v", err)
	}
	waitForIngestDrain(t, st)

	got, err := c.Extractions(t.Context(), wire.ExtractionsRequest{Kind: events.ExtractionKindPRCreated, Value: pr})
	if err != nil {
		t.Fatalf("Extractions(pr_created): %v", err)
	}
	cwd := "/work/creator"
	want := []wire.ExtractionSighting{{
		SessionID:  events.DeriveSessionID("claude-code", "creator"),
		Kind:       events.ExtractionKindPRCreated,
		Value:      pr,
		TsSourceMs: created.UnixMilli(),
		Cwd:        &cwd,
	}}
	if !reflect.DeepEqual(got.Extractions, want) {
		t.Errorf("pr_created: got %+v, want %+v", got.Extractions, want)
	}

	got, err = c.Extractions(t.Context(), wire.ExtractionsRequest{Kind: events.ExtractionKindURL, Value: tricky})
	if err != nil {
		t.Fatalf("Extractions(url): %v", err)
	}
	if len(got.Extractions) != 1 || got.Extractions[0].Value != tricky ||
		got.Extractions[0].SessionID != events.DeriveSessionID("claude-code", "mentioner") {
		t.Errorf("url lookup by %q: got %+v", tricky, got.Extractions)
	}
}

func TestClient_Extractions_MissingKindIs400(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)
	_, err := c.Extractions(t.Context(), wire.ExtractionsRequest{Value: "https://g/o/r/pull/1"})
	var herr *HTTPError
	if !errors.As(err, &herr) || herr.Status != http.StatusBadRequest {
		t.Fatalf("got %v, want *HTTPError with status 400", err)
	}
}
