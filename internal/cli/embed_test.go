package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
)

// fakeEmbedder is a deterministic stand-in for an OpenAI client.
// Returns Vec[i] = [hash(input[i]), len(input[i])] in float32, which
// is enough variety for the loop tests to verify ordering and
// per-row persistence without speaking the network.
type fakeEmbedder struct {
	calls  int
	inputs [][]string
	model  string
	dim    int
	err    error
}

func (f *fakeEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	f.calls++
	f.inputs = append(f.inputs, append([]string(nil), req.Inputs...))
	if f.err != nil {
		return nil, f.err
	}
	dim := f.dim
	if dim == 0 {
		dim = 2
	}
	out := make([][]float32, len(req.Inputs))
	for i, s := range req.Inputs {
		v := make([]float32, dim)
		v[0] = float32(len(s)) // 1st dim: length
		if dim > 1 {
			v[1] = float32(stableHash(s)) // 2nd dim: cheap hash
		}
		out[i] = v
	}
	if f.model == "" {
		f.model = req.Model
	}
	return &llm.EmbedResponse{
		Model:   req.Model,
		Vectors: out,
		Usage:   llm.Usage{InputTokens: len(req.Inputs)},
	}, nil
}

// stableHash is a small deterministic mixer so test fixtures don't
// rely on a real hash. Cheap, not cryptographic.
func stableHash(s string) int32 {
	var h int32 = 17
	for _, r := range s {
		h = h*31 + int32(r)
	}
	return h
}

func TestRunEmbedLoop_EmbedsAllCandidatesIdempotent(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	emb := &fakeEmbedder{dim: 3}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	out, err := runEmbedLoop(context.Background(), s, emb, "test-model", 2,
		store.EmbeddingCandidateFilter{Model: "test-model"}, log)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !strings.Contains(out, "embedded ") {
		t.Errorf("summary: %q", out)
	}
	// seedStore inserts 4 rows, all with non-empty content.
	missing, _ := store.CountMissingEmbeddings(context.Background(), s.DB(),
		store.EmbeddingCandidateFilter{Model: "test-model"})
	if missing != 0 {
		t.Errorf("missing after run: got %d, want 0", missing)
	}

	// Second run is a no-op (everything is embedded).
	emb2 := &fakeEmbedder{dim: 3}
	if _, err := runEmbedLoop(context.Background(), s, emb2, "test-model", 2,
		store.EmbeddingCandidateFilter{Model: "test-model"}, log); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if emb2.calls != 0 {
		t.Errorf("second run should not call embedder: got %d calls", emb2.calls)
	}
}

func TestRunEmbedLoop_RespectsFilterLimit(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	emb := &fakeEmbedder{dim: 2}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := runEmbedLoop(context.Background(), s, emb, "test-model", 10,
		store.EmbeddingCandidateFilter{Model: "test-model", Limit: 2}, log); err != nil {
		t.Fatalf("run: %v", err)
	}
	missing, _ := store.CountMissingEmbeddings(context.Background(), s.DB(),
		store.EmbeddingCandidateFilter{Model: "test-model"})
	// 4 seeded - 2 limit = 2 still missing.
	if missing != 2 {
		t.Errorf("missing after limited run: got %d want 2", missing)
	}
}

func TestRunEmbedLoop_BatchesAcrossPages(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	emb := &fakeEmbedder{dim: 2}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := runEmbedLoop(context.Background(), s, emb, "test-model", 2,
		store.EmbeddingCandidateFilter{Model: "test-model"}, log); err != nil {
		t.Fatalf("run: %v", err)
	}
	// 4 rows, batch=2 → exactly 2 calls.
	if emb.calls != 2 {
		t.Errorf("expected 2 batches, got %d", emb.calls)
	}
}

func TestRunEmbedLoop_PropagatesEmbedderError(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	emb := &fakeEmbedder{err: errors.New("boom")}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	_, err := runEmbedLoop(context.Background(), s, emb, "test-model", 4,
		store.EmbeddingCandidateFilter{Model: "test-model"}, log)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected boom error, got %v", err)
	}
}

func TestTruncateForEmbedding_CapsAtRuneLimit(t *testing.T) {
	t.Parallel()
	short := "hello"
	if got := truncateForEmbedding(short); got != short {
		t.Errorf("short string mutated: %q", got)
	}
	long := strings.Repeat("x", embedSnippetRunes+100)
	got := truncateForEmbedding(long)
	if r := []rune(got); len(r) != embedSnippetRunes {
		t.Errorf("long string not truncated to %d runes: got %d", embedSnippetRunes, len(r))
	}
}

func TestRunSemanticSearch_EmbedsQueryAndRanks(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	emb := &fakeEmbedder{dim: 2}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Pre-populate embeddings for everything in the store.
	if _, err := runEmbedLoop(context.Background(), s, emb, "test-model", 8,
		store.EmbeddingCandidateFilter{Model: "test-model"}, log); err != nil {
		t.Fatalf("seed embed: %v", err)
	}

	var buf strings.Builder
	err := RunSemanticSearch(context.Background(), s, SearchOptions{
		Query:          "json",
		EmbeddingModel: "test-model",
		Limit:          5,
		Format:         FormatTable,
	},
		func() (llm.Embedder, error) { return emb, nil },
		&buf)
	if err != nil {
		t.Fatalf("RunSemanticSearch: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "SCORE") {
		t.Errorf("output missing header: %q", got)
	}
	// 4 events; we asked for 5; expect 4 rows + header.
	if lines := strings.Count(got, "\n"); lines < 5 {
		t.Errorf("expected ≥5 lines, got %d:\n%s", lines, got)
	}
}

func TestRunSemanticSearch_NoHitsForUnknownModel(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	emb := &fakeEmbedder{dim: 2}
	var buf strings.Builder
	err := RunSemanticSearch(context.Background(), s, SearchOptions{
		Query:          "anything",
		EmbeddingModel: "nothing-stored",
		Limit:          5,
		Format:         FormatTable,
	},
		func() (llm.Embedder, error) { return emb, nil },
		&buf)
	if err != nil {
		t.Fatalf("RunSemanticSearch: %v", err)
	}
	if !strings.Contains(buf.String(), "no semantic hits") {
		t.Errorf("expected no-hits line, got %q", buf.String())
	}
}

func TestRunSemanticSearch_RejectsEmptyQuery(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	err := RunSemanticSearch(context.Background(), s, SearchOptions{Query: "  "},
		func() (llm.Embedder, error) { return &fakeEmbedder{dim: 2}, nil },
		io.Discard)
	if err == nil {
		t.Error("expected error on empty query")
	}
}

func TestEmbedRunSummary_FormatStable(t *testing.T) {
	t.Parallel()
	s := embedRunSummary{EmbeddedRows: 7, BatchesRun: 2, InputTokens: 1234}
	got := s.String()
	for _, want := range []string{"embedded 7 rows", "2 batches", "1234 input tokens"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}
