package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOpenAIEmbedder mirrors fakeOpenAI, swapping in an
// OpenAIEmbedder. Same Content-Type/JSON wiring.
func fakeOpenAIEmbedder(t *testing.T, handler http.HandlerFunc) *OpenAIEmbedder {
	t.Helper()
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}
	srv := httptest.NewServer(http.HandlerFunc(wrapped))
	t.Cleanup(srv.Close)
	return &OpenAIEmbedder{
		APIKey:   "test-openai-key",
		Endpoint: srv.URL,
		HTTP:     srv.Client(),
	}
}

func TestOpenAIEmbedder_Embed_HappyPath(t *testing.T) {
	t.Parallel()
	var gotPath string
	e := fakeOpenAIEmbedder(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{
			"object":"list",
			"model":"text-embedding-3-small",
			"data":[
				{"object":"embedding","index":0,"embedding":[0.1, 0.2, 0.3]},
				{"object":"embedding","index":1,"embedding":[-0.4, 0.5, 0.6]}
			],
			"usage":{"prompt_tokens":7,"total_tokens":7}
		}`)
	})

	resp, err := e.Embed(context.Background(), EmbedRequest{
		Inputs: []string{"hello", "world"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/embeddings") {
		t.Errorf("path: got %q, want ending with /embeddings", gotPath)
	}
	if len(resp.Vectors) != 2 {
		t.Fatalf("vectors: got %d", len(resp.Vectors))
	}
	want0 := []float32{0.1, 0.2, 0.3}
	for i := range want0 {
		if resp.Vectors[0][i] != want0[i] {
			t.Errorf("vec[0][%d]: got %v want %v", i, resp.Vectors[0][i], want0[i])
		}
	}
	if resp.Usage.InputTokens != 7 {
		t.Errorf("usage InputTokens: %d", resp.Usage.InputTokens)
	}
}

func TestOpenAIEmbedder_Embed_AssignsByIndex(t *testing.T) {
	t.Parallel()
	// API returns data out of order — Embed must rebuild in input order.
	e := fakeOpenAIEmbedder(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"object":"list",
			"model":"text-embedding-3-small",
			"data":[
				{"object":"embedding","index":1,"embedding":[2.0]},
				{"object":"embedding","index":0,"embedding":[1.0]}
			],
			"usage":{"prompt_tokens":2,"total_tokens":2}
		}`)
	})
	resp, err := e.Embed(context.Background(), EmbedRequest{Inputs: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if resp.Vectors[0][0] != 1.0 || resp.Vectors[1][0] != 2.0 {
		t.Errorf("ordering by .Index lost: %v", resp.Vectors)
	}
}

func TestOpenAIEmbedder_Embed_RejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	e := &OpenAIEmbedder{APIKey: "k"}
	if _, err := e.Embed(context.Background(), EmbedRequest{}); err == nil {
		t.Error("expected error for zero inputs")
	}
	if _, err := e.Embed(context.Background(), EmbedRequest{Inputs: []string{"a", ""}}); err == nil {
		t.Error("expected error for blank input string")
	}
}

func TestOpenAIEmbedder_Embed_RejectsMissingKey(t *testing.T) {
	t.Parallel()
	e := &OpenAIEmbedder{}
	_, err := e.Embed(context.Background(), EmbedRequest{Inputs: []string{"x"}})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Errorf("want missing-key error, got %v", err)
	}
}

func TestOpenAIEmbedder_Embed_LengthMismatchReturnsError(t *testing.T) {
	t.Parallel()
	e := fakeOpenAIEmbedder(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"object":"list",
			"model":"text-embedding-3-small",
			"data":[{"object":"embedding","index":0,"embedding":[1.0]}],
			"usage":{"prompt_tokens":2,"total_tokens":2}
		}`)
	})
	_, err := e.Embed(context.Background(), EmbedRequest{Inputs: []string{"a", "b"}})
	if err == nil {
		t.Error("expected length-mismatch error")
	}
}

func TestEmbedderFromConfig_AnthropicStillRoutesToOpenAI(t *testing.T) {
	// t.Setenv conflicts with t.Parallel(); kept serial deliberately.
	// Embeddings always route through OpenAI — Anthropic has no
	// embeddings endpoint, so a user running provider=anthropic for
	// completions must still configure OpenAI for this path. With
	// neither env nor api_key_command set, surface the OpenAI-side
	// "key not set" error rather than refusing on provider grounds.
	t.Setenv(OpenAIAPIKeyEnv, "")
	_, err := EmbedderFromConfig(context.Background(), Config{Provider: ProviderAnthropic})
	if err == nil {
		t.Fatal("expected error when no OpenAI key is configured")
	}
	if !strings.Contains(err.Error(), OpenAIAPIKeyEnv) {
		t.Errorf("expected OpenAI key error, got %v", err)
	}
}

func TestEmbedderFromConfig_AnthropicWithOpenAIEnvWorks(t *testing.T) {
	// Serial: t.Setenv mutates process env.
	t.Setenv(OpenAIAPIKeyEnv, "test-key")
	got, err := EmbedderFromConfig(context.Background(), Config{Provider: ProviderAnthropic})
	if err != nil {
		t.Fatalf("anthropic + OPENAI env: %v", err)
	}
	if got == nil {
		t.Error("expected non-nil embedder")
	}
}

func TestEmbedderFromConfig_RejectsUnknownProvider(t *testing.T) {
	t.Parallel()
	_, err := EmbedderFromConfig(context.Background(), Config{Provider: "weird"})
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("expected unknown-provider error, got %v", err)
	}
}
