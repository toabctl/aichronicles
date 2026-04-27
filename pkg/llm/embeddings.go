package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"

	openaisdk "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// EmbedderModel names a hosted embedding model. Strings mirror the
// provider's identifier so callers can pass user-supplied values
// straight through.
type EmbedderModel string

// DefaultEmbeddingModel is the model EmbedTexts falls back to when
// EmbedRequest.Model is empty. text-embedding-3-small is OpenAI's
// $0.02/1M-tokens / 1536-dim option — the cheapest of the three
// hosted models that still produces useful semantic vectors. Bigger
// models (text-embedding-3-large) double the cost without doubling
// retrieval quality on our content.
const DefaultEmbeddingModel = "text-embedding-3-small"

// DefaultEmbeddingDim is the dimensionality of DefaultEmbeddingModel.
// Exposed so callers wiring up the store layer don't have to hard-
// code 1536 in two places. Pinned because OpenAI lets you request
// fewer dims via the `dimensions` parameter, and we currently don't.
const DefaultEmbeddingDim = 1536

// EmbedRequest is the provider-neutral input shape for batched
// embedding calls. Inputs must all be non-empty; the embedder rejects
// empty strings before issuing the API call so a stray blank doesn't
// silently consume a quota slot.
type EmbedRequest struct {
	Model  string
	Inputs []string
}

// EmbedResponse mirrors openai.CreateEmbeddingResponse with shapes
// trimmed to what the store layer needs. Vectors are []float32 to
// match how we persist them (event_embeddings.vec is 4-bytes-per-dim
// little-endian); the SDK returns []float64 by default and we narrow
// here so the store boundary deals in one type.
type EmbedResponse struct {
	Model   string
	Vectors [][]float32
	Usage   Usage
}

// Embedder is the embedding-side counterpart to Client. Kept as a
// separate interface so a Client implementation that lacks an
// embeddings endpoint (e.g. the future Anthropic adapter, which
// doesn't expose one) doesn't have to pretend.
type Embedder interface {
	Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error)
}

// OpenAIEmbedder is an Embedder backed by github.com/openai/openai-go.
// Construct via NewOpenAIEmbedder or one of the FromEnv* helpers.
// Safe for concurrent use; the SDK client is built once via sdkOnce.
type OpenAIEmbedder struct {
	APIKey   string
	Endpoint string       // overridable for tests, e.g. httptest.URL
	HTTP     *http.Client // overridable for tests

	MaxRetries int

	sdkOnce   sync.Once
	sdkClient openaisdk.Client
}

// NewOpenAIEmbedder returns an embedder ready for production use.
// Empty key is rejected at call time, mirroring NewOpenAI.
func NewOpenAIEmbedder(apiKey string) *OpenAIEmbedder {
	return &OpenAIEmbedder{APIKey: apiKey}
}

// FromEnvOpenAIEmbedder mirrors FromEnvOpenAI: $OPENAI_API_KEY only,
// no shell-command fallback. Use FromEnvOrCommandOpenAIEmbedder for
// the full precedence.
func FromEnvOpenAIEmbedder() (Embedder, error) {
	key := os.Getenv(OpenAIAPIKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("llm: %s not set", OpenAIAPIKeyEnv)
	}
	return NewOpenAIEmbedder(key), nil
}

// FromEnvOrCommandOpenAIEmbedder mirrors FromEnvOrCommandOpenAI:
// env first, then the optional shell command. Same rules.
func FromEnvOrCommandOpenAIEmbedder(ctx context.Context, command string) (Embedder, error) {
	if key := os.Getenv(OpenAIAPIKeyEnv); key != "" {
		return NewOpenAIEmbedder(key), nil
	}
	if command == "" {
		return nil, fmt.Errorf("llm: %s not set and no api_key_command configured", OpenAIAPIKeyEnv)
	}
	key, err := runKeyCommand(ctx, command)
	if err != nil {
		return nil, err
	}
	return NewOpenAIEmbedder(key), nil
}

// EmbedderFromConfig is the canonical entry for callers wiring up
// embedding support from the [llm] config block.
//
// Embeddings always route through OpenAI regardless of cfg.Provider:
// Anthropic does not expose a hosted embeddings endpoint, so a user
// running provider=anthropic for completions still needs an OpenAI
// key for this path. We use cfg.OpenAI.APIKeyCommand if set, falling
// back to $OPENAI_API_KEY. The error surfaces clearly when neither
// is configured.
func EmbedderFromConfig(ctx context.Context, cfg Config) (Embedder, error) {
	if cfg.Provider != "" && cfg.Provider != ProviderAnthropic && cfg.Provider != ProviderOpenAI {
		return nil, fmt.Errorf("llm: unknown provider %q", cfg.Provider)
	}
	return FromEnvOrCommandOpenAIEmbedder(ctx, cfg.OpenAI.APIKeyCommand)
}

// ensureSDK lazily builds the SDK client. Mirrors OpenAI.ensureSDK.
func (o *OpenAIEmbedder) ensureSDK() {
	o.sdkOnce.Do(func() {
		var opts []option.RequestOption
		opts = append(opts, option.WithAPIKey(o.APIKey))
		if o.Endpoint != "" {
			opts = append(opts, option.WithBaseURL(o.Endpoint))
		}
		if o.HTTP != nil {
			opts = append(opts, option.WithHTTPClient(o.HTTP))
		}
		retries := o.MaxRetries
		if retries == 0 {
			retries = DefaultMaxRetries
		}
		if retries < 0 {
			retries = 0
		}
		opts = append(opts, option.WithMaxRetries(retries))
		o.sdkClient = openaisdk.NewClient(opts...)
	})
}

// Embed sends Inputs to OpenAI's embeddings endpoint as a single
// batch and returns the resulting vectors in the same order. The
// caller is responsible for chunking inputs if the request would
// exceed the per-call token budget — text-embedding-3-small accepts
// up to 8192 tokens per input and 2048 inputs per request, which is
// plenty for our typical batch sizes.
//
// Empty Inputs is rejected (no point paying for a request that does
// nothing). Empty strings inside Inputs are also rejected — the API
// returns 400 for them and a single bad input fails the whole batch,
// which is worse than the explicit check.
func (o *OpenAIEmbedder) Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error) {
	if o.APIKey == "" {
		return nil, errors.New("openai: API key not set (expected in OPENAI_API_KEY)")
	}
	if len(req.Inputs) == 0 {
		return nil, errors.New("openai: Embed: no inputs")
	}
	for i, s := range req.Inputs {
		if s == "" {
			return nil, fmt.Errorf("openai: Embed: input[%d] is empty", i)
		}
	}
	model := req.Model
	if model == "" {
		model = DefaultEmbeddingModel
	}

	o.ensureSDK()
	resp, err := o.sdkClient.Embeddings.New(ctx, openaisdk.EmbeddingNewParams{
		Model: openaisdk.EmbeddingModel(model),
		Input: openaisdk.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: req.Inputs,
		},
		EncodingFormat: openaisdk.EmbeddingNewParamsEncodingFormatFloat,
	})
	if err != nil {
		return nil, scrubOpenAIError(err)
	}
	if len(resp.Data) != len(req.Inputs) {
		return nil, fmt.Errorf("openai: embed: got %d vectors for %d inputs",
			len(resp.Data), len(req.Inputs))
	}

	out := &EmbedResponse{
		Model:   resp.Model,
		Vectors: make([][]float32, len(resp.Data)),
		Usage: Usage{
			InputTokens:  int(resp.Usage.PromptTokens),
			OutputTokens: 0, // embeddings have no output tokens
		},
	}
	// Per docs the API echoes the input position via .Index. Assign by
	// that field rather than the slice order, in case a future version
	// reorders for streaming or sharding.
	for _, e := range resp.Data {
		idx := int(e.Index)
		if idx < 0 || idx >= len(req.Inputs) {
			return nil, fmt.Errorf("openai: embed: bad index %d", idx)
		}
		out.Vectors[idx] = float64sToFloat32s(e.Embedding)
	}
	return out, nil
}

// float64sToFloat32s narrows OpenAI's response (declared as []float64
// in the SDK) to the float32 representation we persist. Lossy in the
// general case but we ship vectors at float32 precision for storage
// and comparison anyway, so the conversion is the right place to pay
// the precision cost — once.
func float64sToFloat32s(in []float64) []float32 {
	out := make([]float32, len(in))
	for i, v := range in {
		out[i] = float32(v)
	}
	return out
}
