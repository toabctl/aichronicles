package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
)

// fakeEmbedder is the minimal in-package Embedder stand-in for
// semantic_search_events tests. Returns a deterministic vector per
// input so the test can seed event_embeddings against a fixed query
// vector and assert ranking.
type fakeEmbedder struct {
	vec    []float32
	called int
	err    error
}

func (f *fakeEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	f.called++
	if f.err != nil {
		return nil, f.err
	}
	out := &llm.EmbedResponse{Model: req.Model}
	for range req.Inputs {
		out.Vectors = append(out.Vectors, f.vec)
	}
	return out, nil
}

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
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesLLMTools(srv, st,
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
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesLLMTools(srv, st, func() (llm.Client, error) { return client, nil })

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
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesLLMTools(srv, st, func() (llm.Client, error) { return client, nil })

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

func TestRegisterAichroniclesEmbeddingTools_RegistersSemanticSearch(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesEmbeddingTools(srv, st,
		func() (llm.Embedder, error) { return &fakeEmbedder{vec: []float32{1, 0, 0}}, nil })

	if _, ok := srv.tools["semantic_search_events"]; !ok {
		t.Error("semantic_search_events not registered")
	}
}

// TestSemanticSearchEvents_NoEmbeddings: with no rows in
// event_embeddings, the tool returns the "(no hits)" hint without a
// JSON-RPC error so the agent knows to suggest `aichronicles embed`.
func TestSemanticSearchEvents_NoEmbeddings(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	embedder := &fakeEmbedder{vec: []float32{1, 0, 0}}
	RegisterAichroniclesEmbeddingTools(srv, st,
		func() (llm.Embedder, error) { return embedder, nil })

	res := callTool(t, srv, "semantic_search_events", `{"query":"jsonl parsing"}`)
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty result")
	}
	if !strings.Contains(res.Content[0].Text, "no hits") {
		t.Errorf("expected '(no hits)' message, got %q", res.Content[0].Text)
	}
	if embedder.called != 1 {
		t.Errorf("embedder called %d times, want 1 (we still embed even when there's nothing to compare against — the empty pool is a separate signal)", embedder.called)
	}
}

// TestSemanticSearchEvents_EmptyQueryRejected: bare-string-empty
// or whitespace-only query is a user error; surface as TextError so
// the agent corrects, not a protocol error.
func TestSemanticSearchEvents_EmptyQueryRejected(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	embedder := &fakeEmbedder{vec: []float32{1, 0, 0}}
	RegisterAichroniclesEmbeddingTools(srv, st,
		func() (llm.Embedder, error) { return embedder, nil })

	res := callTool(t, srv, "semantic_search_events", `{"query":"  "}`)
	if res == nil || len(res.Content) == 0 ||
		!strings.Contains(res.Content[0].Text, "query is required") {
		t.Errorf("expected 'query is required', got %+v", res)
	}
	if embedder.called != 0 {
		t.Errorf("embedder called on empty query: count=%d", embedder.called)
	}
}

// TestSemanticSearchEvents_RanksAndReturnsHits: seed embeddings on
// the events from openSeededStore, ask the embedder to return a
// vector aligned with the "jsonl" event's embedding, and assert the
// tool returns that event first. The exact ordering matters less
// than (a) it returns a non-empty result and (b) the matching event
// outranks the unrelated one.
func TestSemanticSearchEvents_RanksAndReturnsHits(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	ctx := context.Background()

	// Pick the two seeded events: jsonl-ish and systemd-ish. We
	// hand-craft 3-dim embeddings so we can pick a query vector
	// that's clearly closer to one than the other.
	rows, err := st.DB().QueryContext(ctx,
		`SELECT event_id, content_text FROM events ORDER BY ts_source_ms ASC`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	type seed struct {
		id   string
		text string
	}
	var seeds []seed
	for rows.Next() {
		var s seed
		var ct strings_NullableString // sql.NullString surrogate; using local helper
		if err := rows.Scan(&s.id, &ct); err != nil {
			t.Fatalf("scan: %v", err)
		}
		s.text = ct.String
		seeds = append(seeds, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(seeds) < 2 {
		t.Fatalf("expected >=2 seeded events, got %d", len(seeds))
	}

	// Embed each event with a hand-crafted 3-dim vector under the
	// default embedding model name (the tool hard-codes that
	// model). The first event ("jsonl" content) gets [1,0,0]; the
	// second ("systemd") gets [0,1,0]; pad the rest with [0,0,1].
	jsonlEventID := ""
	for i, sd := range seeds {
		vec := []float32{0, 0, 0}
		switch {
		case strings.Contains(sd.text, "jsonl") || strings.Contains(sd.text, "bufio"):
			vec[0] = 1
			jsonlEventID = sd.id
		case strings.Contains(sd.text, "systemd") || strings.Contains(sd.text, "LISTEN_FDS"):
			vec[1] = 1
		default:
			vec[2] = 1
		}
		if err := store.SaveEmbedding(ctx, st.DB(), store.Embedding{
			EventID:     sd.id,
			Model:       string(llm.DefaultEmbeddingModel),
			Dim:         3,
			Vec:         vec,
			CreatedAtMs: int64(1_700_000_000_000 + i),
		}); err != nil {
			t.Fatalf("seed embedding %d: %v", i, err)
		}
	}
	if jsonlEventID == "" {
		t.Fatalf("did not find a jsonl-shaped seed event")
	}

	// Embedder responds with a vector aligned to the jsonl events.
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	embedder := &fakeEmbedder{vec: []float32{1, 0, 0}}
	RegisterAichroniclesEmbeddingTools(srv, st,
		func() (llm.Embedder, error) { return embedder, nil })

	res := callTool(t, srv, "semantic_search_events", `{"query":"jsonl parsing"}`)
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty result")
	}
	body := res.Content[0].Text
	if strings.Contains(body, "no hits") {
		t.Fatalf("expected hits but got '(no hits)':\n%s", body)
	}

	// jsonl event must rank first — its embedding is the same
	// direction as the query vector (cosine = 1.0), the systemd
	// event's embedding is orthogonal (cosine = 0).
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected ≥2 hit lines (one per seeded event), got %d:\n%s", len(lines), body)
	}
	first := lines[0]
	// Each output line is "session8\t...". The first line should
	// carry score 1.000 — the jsonl event's embedding is exactly
	// the query vector, so cosine = 1.0. Other events have
	// orthogonal embeddings (cosine = 0).
	if !strings.Contains(first, "1.000") {
		t.Errorf("first hit's score should be 1.000 (perfect cosine), got line: %q", first)
	}
	_ = jsonlEventID // retained as a sanity capture; rendered hits don't carry the full id
	if embedder.called != 1 {
		t.Errorf("embedder called %d times, want exactly 1", embedder.called)
	}
}

// strings_NullableString shims sql.NullString for Scan in this test
// without dragging the database/sql import into the test header
// solely for one usage.
type strings_NullableString struct {
	String string
	Valid  bool
}

func (s *strings_NullableString) Scan(src any) error {
	if src == nil {
		s.String, s.Valid = "", false
		return nil
	}
	if v, ok := src.(string); ok {
		s.String, s.Valid = v, true
		return nil
	}
	return errors.New("unsupported scan src for strings_NullableString")
}

// TestSemanticSearchEvents_EmbedderFailureBubblesAsTextResult: an
// embedder construction failure becomes a tool-level TextError, not
// a JSON-RPC protocol error — agents can read the message and fall
// back to plain search_events.
func TestSemanticSearchEvents_EmbedderFailureBubblesAsTextResult(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesEmbeddingTools(srv, st,
		func() (llm.Embedder, error) { return nil, errors.New("no api key") })

	res := callTool(t, srv, "semantic_search_events", `{"query":"anything"}`)
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty result")
	}
	if !strings.Contains(res.Content[0].Text, "embedder unavailable") {
		t.Errorf("expected embedder-unavailable message, got %q", res.Content[0].Text)
	}
}

// TestSearchWithSummary_LLMErrorBubblesAsTextResult confirms an
// LLM-client construction failure becomes a user-facing tool reply
// (so the agent can adapt) rather than a JSON-RPC protocol error.
func TestSearchWithSummary_LLMErrorBubblesAsTextResult(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesLLMTools(srv, st,
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
	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesLLMTools(srv, st, func() (llm.Client, error) { return client, nil })

	res := callTool(t, srv, "search_with_summary", `{"query":"jsonl","top_n":999}`)
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty result")
	}
	if client.called != 1 {
		t.Errorf("LLM call count: got %d, want 1", client.called)
	}
}
