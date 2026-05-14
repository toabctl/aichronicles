package mcp

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/llm"
)

// fakeLLMClient is a tiny in-package stand-in for the LLM client.
// search_with_summary uses free-text Response.Text (no tool
// forcing), so we don't need the full fake-with-toolinput dance
// that the cli package's fakeLLM does.
type fakeLLMClient struct {
	reply  string
	called int
	err    error
}

func (f *fakeLLMClient) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	f.called++
	if f.err != nil {
		return nil, f.err
	}
	return &llm.Response{Text: f.reply, Model: "fake-model"}, nil
}

func TestRegisterAichroniclesLLMTools_RegistersSearchWithSummary(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, slog.New(slog.DiscardHandler))
	RegisterAichroniclesLLMTools(srv, newAPITestClient(t, st),
		func() (llm.Client, error) { return &fakeLLMClient{}, nil })

	if _, ok := srv.tools["search_with_summary"]; !ok {
		t.Error("search_with_summary not registered")
	}
}

// TestSearchWithSummary_NoHits returns "(no hits)" without calling
// the LLM, so an agent doesn't burn tokens on empty searches.
func TestSearchWithSummary_NoHits(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)

	client := &fakeLLMClient{reply: "should not be reached"}
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, slog.New(slog.DiscardHandler))
	RegisterAichroniclesLLMTools(srv, newAPITestClient(t, st), func() (llm.Client, error) { return client, nil })

	res := callTool(t, srv, "search_with_summary", `{"query":"nothingmatchesatallzzz"}`)
	if res == nil || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "(no hits)") {
		t.Errorf("expected '(no hits)', got %+v", res)
	}
	if client.called != 0 {
		t.Errorf("LLM called on empty hits: count=%d", client.called)
	}
}

// TestSearchWithSummary_GroundsAndCites confirms the LLM gets the
// hit content as grounding and the response carries the
// "Grounded in:" citations footer.
func TestSearchWithSummary_GroundsAndCites(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	client := &fakeLLMClient{reply: "Bufio works for jsonl, see [session=abc12345]."}
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, slog.New(slog.DiscardHandler))
	RegisterAichroniclesLLMTools(srv, newAPITestClient(t, st), func() (llm.Client, error) { return client, nil })

	res := callTool(t, srv, "search_with_summary", `{"query":"jsonl"}`)
	if client.called != 1 {
		t.Fatalf("LLM call count: got %d, want 1", client.called)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty result")
	}
	body := res.Content[0].Text
	if !strings.Contains(body, "Bufio works for jsonl") {
		t.Errorf("answer missing in result: %s", body)
	}
	if !strings.Contains(body, "Grounded in:") {
		t.Errorf("citations footer missing: %s", body)
	}
}

// TestSearchWithSummary_LLMErrorBubblesAsTextResult confirms an
// LLM-client construction failure becomes a user-facing tool reply
// (so the agent can adapt) rather than a JSON-RPC protocol error.
func TestSearchWithSummary_LLMErrorBubblesAsTextResult(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, slog.New(slog.DiscardHandler))
	RegisterAichroniclesLLMTools(srv, newAPITestClient(t, st),
		func() (llm.Client, error) { return nil, errors.New("no api key") })

	res := callTool(t, srv, "search_with_summary", `{"query":"jsonl"}`)
	if res == nil || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "LLM client unavailable") {
		t.Errorf("expected LLM-unavailable text result, got %+v", res)
	}
}

// TestSearchWithSummary_TopNCappedAtMax pins the cap: even when an
// agent passes top_n=999, the registered schema's maximum (10) is
// enforced — both via JSON-Schema upstream and the runtime cap in
// the handler. We assert the handler still succeeds rather than
// erroring on out-of-range input.
func TestSearchWithSummary_TopNCappedAtMax(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	client := &fakeLLMClient{reply: "ok"}
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, slog.New(slog.DiscardHandler))
	RegisterAichroniclesLLMTools(srv, newAPITestClient(t, st), func() (llm.Client, error) { return client, nil })

	res := callTool(t, srv, "search_with_summary", `{"query":"jsonl","top_n":999}`)
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty result")
	}
	if client.called != 1 {
		t.Errorf("LLM call count: got %d, want 1", client.called)
	}
}
