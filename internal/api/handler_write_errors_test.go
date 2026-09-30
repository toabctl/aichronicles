package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/wire"
)

// writeFixture seeds one real session and one LLM output so write
// tests can reference ids that exist next to ones that don't.
type writeFixture struct {
	srv       *testServer
	sessionID string
	outputID  int64
}

func newWriteFixture(t *testing.T) writeFixture {
	t.Helper()
	srv := newTestServer(t)
	env := validEnvelope(t)
	if code, body := postJSON(t, srv, "/v1/ingest", env); code != http.StatusOK {
		t.Fatalf("seed ingest: %d %s", code, body)
	}
	sid := events.DeriveSessionID(env.SourceAgent, env.SourceSessionID)
	code, body := postJSON(t, srv, "/v1/llm-outputs", wire.SaveLLMOutputRequest{
		SessionID: &sid, Kind: "facts", Model: "m", PromptHash: "h", Body: "{}", CreatedAtMs: 1,
	})
	if code != http.StatusOK {
		t.Fatalf("seed llm output: %d %s", code, body)
	}
	var out wire.SaveLLMOutputResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return writeFixture{srv: srv, sessionID: sid, outputID: out.ID}
}

func postJSON(t *testing.T, srv *testServer, path string, v any) (int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, bytesReader(mustJSON(t, v))))
	return rr.Code, rr.Body.String()
}

// TestWrites_RejectedInputIs400 is the regression gate for write
// handlers answering 500 "Storage error" to input the store refused:
// unknown ids (foreign keys), out-of-range or missing values (store
// validation) and CHECK / trigger violations. Each is the caller's
// mistake and must come back 400 with a title naming the class.
func TestWrites_RejectedInputIs400(t *testing.T) {
	t.Parallel()
	fx := newWriteFixture(t)
	ghost := "00000000-0000-0000-0000-00000000dead"
	fact := func(mut func(*wire.SaveSemanticFactRequest)) wire.SaveSemanticFactRequest {
		r := wire.SaveSemanticFactRequest{
			SourceLLMOutputID: fx.outputID, Subject: "/work/x", Predicate: "primary_language",
			Object: "Go", Confidence: 0.9, AssertedAtMs: 1_760_000_000_000,
		}
		mut(&r)
		return r
	}
	cases := []struct {
		name, path string
		body       any
		wantTitle  string
	}{
		{"fact: empty object", "/v1/facts", fact(func(r *wire.SaveSemanticFactRequest) { r.Object = "" }), "Invalid value"},
		{"fact: confidence > 1", "/v1/facts", fact(func(r *wire.SaveSemanticFactRequest) { r.Confidence = 2 }), "Invalid value"},
		{"fact: asserted_at_ms 0", "/v1/facts", fact(func(r *wire.SaveSemanticFactRequest) { r.AssertedAtMs = 0 }), "Invalid value"},
		{"fact: unknown llm output", "/v1/facts", fact(func(r *wire.SaveSemanticFactRequest) { r.SourceLLMOutputID = 999_999 }), "Unknown reference"},
		{"outcome: unknown session", "/v1/session-outcomes", wire.SaveSessionOutcomeRequest{SessionID: ghost, ComputedAtMs: 1, Outcome: "unknown"}, "Unknown reference"},
		{"outcome: empty session", "/v1/session-outcomes", wire.SaveSessionOutcomeRequest{ComputedAtMs: 1, Outcome: "unknown"}, "Invalid value"},
		{"outcome: bogus label", "/v1/session-outcomes", wire.SaveSessionOutcomeRequest{SessionID: fx.sessionID, ComputedAtMs: 1, Outcome: "bogus"}, "Invalid value"},
		{"episodes: unknown session", "/v1/episodes", wire.SaveEpisodesRequest{SessionID: ghost, Episodes: []wire.Episode{{Ordinal: 1, FirstEventID: "e"}}}, "Unknown reference"},
		{"links: unknown target", "/v1/session-links", wire.SaveSessionLinksRequest{FromSessionID: fx.sessionID, Links: []wire.SessionLink{{ToSessionID: ghost, Kind: "related"}}}, "Unknown reference"},
		{"candidate: unknown llm output", "/v1/skill-candidates", wire.RecordSkillCandidateRequest{LLMOutputID: 999_999, SkillName: "s", ProposedAtMs: 1}, "Unknown reference"},
	}
	for _, tc := range cases {
		code, body := postJSON(t, fx.srv, tc.path, tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, `"title":"`+tc.wantTitle+`"`) {
			t.Errorf("%s: got %d %s, want 400 %q", tc.name, code, body, tc.wantTitle)
		}
	}
}

// TestSkillCandidateDecision_InvalidMergeIs400 covers the trigger path:
// merging into a candidate that was never added is refused by a
// RAISE(ABORT) trigger, which used to surface as 500.
func TestSkillCandidateDecision_InvalidMergeIs400(t *testing.T) {
	t.Parallel()
	fx := newWriteFixture(t)
	for _, name := range []string{"target", "merged"} {
		if code, body := postJSON(t, fx.srv, "/v1/skill-candidates", wire.RecordSkillCandidateRequest{
			LLMOutputID: fx.outputID, SkillName: name, ProposedAtMs: 1,
		}); code != http.StatusOK {
			t.Fatalf("record %s: %d %s", name, code, body)
		}
	}
	var target wire.SkillCandidatesResponse
	rr := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/skill-candidates?name=target", nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &target); err != nil || len(target.Candidates) != 1 {
		t.Fatalf("lookup target: %v %s", err, rr.Body.String())
	}
	for name, mergedInto := range map[string]int64{
		"target not added": target.Candidates[0].ID,
		"negative id":      -1,
	} {
		code, body := postJSON(t, fx.srv, "/v1/skill-candidates/decision", wire.SkillCandidateDecisionRequest{
			LLMOutputID: fx.outputID, SkillName: "merged", Decision: wire.DecisionMerge,
			DecisionAtMs: 2, AddPath: "/tmp/SKILL.md", MergedIntoID: mergedInto,
		})
		if code != http.StatusBadRequest || !strings.Contains(body, `"title":"Invalid value"`) {
			t.Errorf("%s: got %d %s, want 400 Invalid value", name, code, body)
		}
	}
}
