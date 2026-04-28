package store

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestEncodeDecodeFloat32Vec_RoundTrips(t *testing.T) {
	t.Parallel()
	in := []float32{0, 1, -1, 0.5, -0.5, float32(math.Pi), float32(math.SmallestNonzeroFloat32), float32(math.MaxFloat32)}
	blob := EncodeFloat32Vec(in)
	if len(blob) != len(in)*4 {
		t.Fatalf("blob len: got %d want %d", len(blob), len(in)*4)
	}
	out, err := DecodeFloat32Vec(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len mismatch: got %d want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("idx %d: got %v want %v", i, out[i], in[i])
		}
	}
}

func TestDecodeFloat32Vec_RejectsBadLength(t *testing.T) {
	t.Parallel()
	_, err := DecodeFloat32Vec([]byte{1, 2, 3}) // not multiple of 4
	if !errors.Is(err, ErrBadEmbeddingBlob) {
		t.Errorf("want ErrBadEmbeddingBlob, got %v", err)
	}
}

func TestSaveEmbedding_Validates(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	cases := []struct {
		name string
		e    Embedding
	}{
		{"missing event_id", Embedding{Model: "m", Dim: 2, Vec: []float32{1, 2}}},
		{"missing model", Embedding{EventID: "e", Dim: 2, Vec: []float32{1, 2}}},
		{"bad dim", Embedding{EventID: "e", Model: "m", Dim: 0, Vec: nil}},
		{"vec/dim mismatch", Embedding{EventID: "e", Model: "m", Dim: 3, Vec: []float32{1, 2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := SaveEmbedding(ctx, s.DB(), tc.e); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestSaveEmbedding_Roundtrip_AndReplaceSemantics(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	insertRawAndEvent(t, s, "e1", "sess-a", "src", "user_prompt", "hello", 100)

	emb := Embedding{
		EventID:     "e1",
		Model:       "test-1",
		Dim:         3,
		Vec:         []float32{1, 0, 0},
		CreatedAtMs: 100,
	}
	if err := SaveEmbedding(ctx, s.DB(), emb); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Re-saving with a different vector REPLACES.
	emb.Vec = []float32{0, 1, 0}
	emb.CreatedAtMs = 200
	if err := SaveEmbedding(ctx, s.DB(), emb); err != nil {
		t.Fatalf("re-save: %v", err)
	}

	var got [][]byte
	rows, err := s.DB().Query(`SELECT vec FROM event_embeddings WHERE event_id='e1'`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, b)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row after replace, got %d", len(got))
	}
	v, err := DecodeFloat32Vec(got[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []float32{0, 1, 0}
	for i := range v {
		if v[i] != want[i] {
			t.Errorf("idx %d: got %v want %v", i, v[i], want[i])
		}
	}
}

func TestSaveEmbedding_DifferentModelsCoexist(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	insertRawAndEvent(t, s, "e1", "sess-a", "src", "user_prompt", "hello", 100)

	if err := SaveEmbedding(ctx, s.DB(), Embedding{
		EventID: "e1", Model: "small", Dim: 2, Vec: []float32{1, 0},
	}); err != nil {
		t.Fatalf("save small: %v", err)
	}
	// Same event_id with a different model should... actually fail
	// because event_id is the PRIMARY KEY. The replace happens on
	// PK conflict, so the small row is gone.
	if err := SaveEmbedding(ctx, s.DB(), Embedding{
		EventID: "e1", Model: "large", Dim: 2, Vec: []float32{0, 1},
	}); err != nil {
		t.Fatalf("save large: %v", err)
	}
	var n int
	_ = s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM event_embeddings WHERE event_id='e1'`).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 row (PK is event_id), got %d", n)
	}
	var model string
	_ = s.DB().QueryRowContext(ctx, `SELECT model FROM event_embeddings WHERE event_id='e1'`).Scan(&model)
	if model != "large" {
		t.Errorf("expected newest model 'large', got %q", model)
	}
}

func TestListEventsWithoutEmbedding_FiltersAndExcludesEmptyText(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	insertRawAndEvent(t, s, "e1", "sess-a", "src", "user_prompt", "alpha", 100)
	insertRawAndEvent(t, s, "e2", "sess-a", "src", "tool_use", "beta", 200)
	insertRawAndEvent(t, s, "e3", "sess-a", "src", "user_prompt", "", 300) // empty content

	// Embed e1 already.
	if err := SaveEmbedding(ctx, s.DB(), Embedding{
		EventID: "e1", Model: "m", Dim: 2, Vec: []float32{1, 0}, CreatedAtMs: 100,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := ListEventsWithoutEmbedding(ctx, s.DB(), EmbeddingCandidateFilter{Model: "m"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// e1 is embedded → skip; e3 has empty content → skip; e2 only.
	if len(got) != 1 || got[0].EventID != "e2" {
		t.Errorf("expected only e2, got %v", got)
	}

	// Same query for a DIFFERENT model: e1 also reappears.
	got2, err := ListEventsWithoutEmbedding(ctx, s.DB(), EmbeddingCandidateFilter{Model: "other"})
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(got2) != 2 {
		t.Errorf("for model=other, expected 2 (e1+e2), got %d: %v", len(got2), got2)
	}

	// Kind filter works.
	got3, err := ListEventsWithoutEmbedding(ctx, s.DB(), EmbeddingCandidateFilter{
		Model: "m", Kinds: []string{"tool_use"},
	})
	if err != nil {
		t.Fatalf("list kind: %v", err)
	}
	if len(got3) != 1 || got3[0].EventID != "e2" {
		t.Errorf("kind filter: got %v", got3)
	}

	// SinceMs filter trims old rows.
	got4, err := ListEventsWithoutEmbedding(ctx, s.DB(), EmbeddingCandidateFilter{
		Model: "m", SinceMs: 250,
	})
	if err != nil {
		t.Fatalf("list since: %v", err)
	}
	// e2 is at ts=200 → below cutoff; e3 has empty content; nothing left.
	if len(got4) != 0 {
		t.Errorf("since filter should drop e2, got %v", got4)
	}
}

func TestListEventsWithoutEmbedding_RequiresModel(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	if _, err := ListEventsWithoutEmbedding(context.Background(), s.DB(), EmbeddingCandidateFilter{}); err == nil {
		t.Error("expected error for empty model")
	}
}

func TestCountMissingEmbeddings_MatchesList(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	insertRawAndEvent(t, s, "e1", "sess-a", "src", "user_prompt", "alpha", 100)
	insertRawAndEvent(t, s, "e2", "sess-a", "src", "tool_use", "beta", 200)

	n, err := CountMissingEmbeddings(ctx, s.DB(), EmbeddingCandidateFilter{Model: "m"})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 missing, got %d", n)
	}

	if err := SaveEmbedding(ctx, s.DB(), Embedding{
		EventID: "e1", Model: "m", Dim: 2, Vec: []float32{1, 0},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	n2, _ := CountMissingEmbeddings(ctx, s.DB(), EmbeddingCandidateFilter{Model: "m"})
	if n2 != 1 {
		t.Errorf("expected 1 missing after embed, got %d", n2)
	}
}

func TestCascade_DeleteEventDropsEmbedding(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	insertRawAndEvent(t, s, "e1", "sess-a", "src", "user_prompt", "alpha", 100)
	if err := SaveEmbedding(ctx, s.DB(), Embedding{
		EventID: "e1", Model: "m", Dim: 2, Vec: []float32{1, 0},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := s.DB().Exec(`DELETE FROM events WHERE event_id='e1'`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM event_embeddings WHERE event_id='e1'`).Scan(&n)
	if n != 0 {
		t.Errorf("embedding should cascade-drop, got %d rows", n)
	}
}

func TestSemanticSearch_OrdersByCosine(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	// Three events with three orthogonal vectors. Querying along x
	// should rank e1 best, then ties among the others.
	insertRawAndEvent(t, s, "e1", "sess-a", "src", "user_prompt", "x-aligned", 100)
	insertRawAndEvent(t, s, "e2", "sess-a", "src", "user_prompt", "y-aligned", 200)
	insertRawAndEvent(t, s, "e3", "sess-a", "src", "user_prompt", "z-aligned", 300)

	for _, e := range []Embedding{
		{EventID: "e1", Model: "m", Dim: 3, Vec: []float32{1, 0, 0}},
		{EventID: "e2", Model: "m", Dim: 3, Vec: []float32{0, 1, 0}},
		{EventID: "e3", Model: "m", Dim: 3, Vec: []float32{0, 0, 1}},
	} {
		if err := SaveEmbedding(ctx, s.DB(), e); err != nil {
			t.Fatalf("save %s: %v", e.EventID, err)
		}
	}

	hits, err := SemanticSearch(ctx, s.DB(), SemanticSearchOpts{
		QueryVec: []float32{0.9, 0.1, 0.1},
		Model:    "m",
		Dim:      3,
		TopK:     3,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("hits: got %d want 3", len(hits))
	}
	if hits[0].EventID != "e1" {
		t.Errorf("top hit: got %s want e1 (most aligned with +x)", hits[0].EventID)
	}
	// Scores should descend.
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Errorf("scores not descending at idx %d: %v", i, hits)
		}
	}
}

func TestSemanticSearch_FiltersByModelAndDim(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	insertRawAndEvent(t, s, "e1", "sess-a", "src", "user_prompt", "small", 100)
	insertRawAndEvent(t, s, "e2", "sess-b", "src2", "user_prompt", "large", 200)

	// e1: model=small, dim=2; e2: model=large, dim=3 (different dim).
	if err := SaveEmbedding(ctx, s.DB(), Embedding{
		EventID: "e1", Model: "small", Dim: 2, Vec: []float32{1, 0},
	}); err != nil {
		t.Fatalf("save e1: %v", err)
	}
	if err := SaveEmbedding(ctx, s.DB(), Embedding{
		EventID: "e2", Model: "large", Dim: 3, Vec: []float32{0, 1, 0},
	}); err != nil {
		t.Fatalf("save e2: %v", err)
	}

	// Query under model=small, dim=2 — only e1 is eligible.
	hits, err := SemanticSearch(ctx, s.DB(), SemanticSearchOpts{
		QueryVec: []float32{1, 0},
		Model:    "small",
		Dim:      2,
		TopK:     5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].EventID != "e1" {
		t.Errorf("expected only e1, got %v", hits)
	}
}

func TestSemanticSearch_TopKCaps(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	for i := range 5 {
		eid := []string{"a", "b", "c", "d", "e"}[i]
		insertRawAndEvent(t, s, eid, "sess-a", "src", "user_prompt", eid, int64(100+i))
		// Each event slightly less aligned with [1,0,0] than the prior.
		v := []float32{1 - 0.1*float32(i), 0.1 * float32(i), 0}
		if err := SaveEmbedding(ctx, s.DB(), Embedding{
			EventID: eid, Model: "m", Dim: 3, Vec: v,
		}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	hits, err := SemanticSearch(ctx, s.DB(), SemanticSearchOpts{
		QueryVec: []float32{1, 0, 0},
		Model:    "m",
		Dim:      3,
		TopK:     2,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("top-K should clamp to 2, got %d", len(hits))
	}
	if hits[0].EventID != "a" || hits[1].EventID != "b" {
		t.Errorf("top 2: got [%s, %s] want [a, b]", hits[0].EventID, hits[1].EventID)
	}
}

func TestLoadFirstPromptEmbedding_FoundOnFirstUserPrompt(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	insertRawAndEvent(t, s, "evt-1", "sess-a", "src", "user_prompt", "first prompt", 100)
	insertRawAndEvent(t, s, "evt-2", "sess-a", "src", "assistant_message", "noise", 200)
	insertRawAndEvent(t, s, "evt-3", "sess-a", "src", "user_prompt", "second prompt", 300)

	if err := SaveEmbedding(ctx, s.DB(), Embedding{
		EventID: "evt-1", Model: "m", Dim: 3, Vec: []float32{1, 0, 0},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := SaveEmbedding(ctx, s.DB(), Embedding{
		EventID: "evt-3", Model: "m", Dim: 3, Vec: []float32{0, 1, 0},
	}); err != nil {
		t.Fatalf("save evt-3: %v", err)
	}

	vec, ok, err := LoadFirstPromptEmbedding(ctx, s.DB(), "sess-a", "m", 3)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	// Must be evt-1's vector (chronologically first user_prompt).
	if vec[0] != 1 || vec[1] != 0 || vec[2] != 0 {
		t.Errorf("vec: got %v want [1 0 0]", vec)
	}
}

func TestLoadFirstPromptEmbedding_MissingPrompt(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	insertRawAndEvent(t, s, "evt-tool", "sess-a", "src", "tool_use", "", 100)

	_, ok, err := LoadFirstPromptEmbedding(context.Background(), s.DB(), "sess-a", "m", 3)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ok {
		t.Error("expected ok=false when no user_prompt event exists")
	}
}

func TestLoadFirstPromptEmbedding_MissingEmbedding(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	insertRawAndEvent(t, s, "evt-1", "sess-a", "src", "user_prompt", "first", 100)
	// No SaveEmbedding call.
	_, ok, err := LoadFirstPromptEmbedding(context.Background(), s.DB(), "sess-a", "m", 3)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ok {
		t.Error("expected ok=false when no embedding exists for the prompt")
	}
}

func TestLoadFirstPromptEmbedding_ModelDimMismatch(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	insertRawAndEvent(t, s, "evt-1", "sess-a", "src", "user_prompt", "first", 100)
	if err := SaveEmbedding(ctx, s.DB(), Embedding{
		EventID: "evt-1", Model: "model-a", Dim: 3, Vec: []float32{1, 0, 0},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Different model — must miss.
	_, ok, err := LoadFirstPromptEmbedding(ctx, s.DB(), "sess-a", "model-b", 3)
	if err != nil {
		t.Fatalf("load (wrong model): %v", err)
	}
	if ok {
		t.Error("expected ok=false on model mismatch")
	}
	// Different dim — must miss.
	_, ok, err = LoadFirstPromptEmbedding(ctx, s.DB(), "sess-a", "model-a", 4)
	if err != nil {
		t.Fatalf("load (wrong dim): %v", err)
	}
	if ok {
		t.Error("expected ok=false on dim mismatch")
	}
}

func TestRerankCandidatesBySimilarity_ScoredFirstThenUnscored(t *testing.T) {
	t.Parallel()
	anchor := []float32{1, 0, 0}
	inputs := []CandidateRerankInput{
		{Candidate: CandidateSession{ID: "y-aligned"}, Vec: []float32{0, 1, 0}},  // cos = 0
		{Candidate: CandidateSession{ID: "x-aligned"}, Vec: []float32{1, 0, 0}},  // cos = 1
		{Candidate: CandidateSession{ID: "no-vec"}, Vec: nil},                    // unembedded
		{Candidate: CandidateSession{ID: "near-x"}, Vec: []float32{0.8, 0.2, 0}}, // high cos
	}
	got := RerankCandidatesBySimilarity(anchor, inputs, 0)
	if len(got) != 4 {
		t.Fatalf("got %d candidates, want 4", len(got))
	}
	// Embedded candidates first, sorted DESC by score:
	if got[0].ID != "x-aligned" {
		t.Errorf("rank 0: got %s want x-aligned", got[0].ID)
	}
	if got[1].ID != "near-x" {
		t.Errorf("rank 1: got %s want near-x", got[1].ID)
	}
	if got[2].ID != "y-aligned" {
		t.Errorf("rank 2: got %s want y-aligned", got[2].ID)
	}
	// Unembedded last:
	if got[3].ID != "no-vec" {
		t.Errorf("rank 3: got %s want no-vec", got[3].ID)
	}
}

func TestRerankCandidatesBySimilarity_FallbackWhenAnchorEmpty(t *testing.T) {
	t.Parallel()
	inputs := []CandidateRerankInput{
		{Candidate: CandidateSession{ID: "first"}, Vec: []float32{1, 0}},
		{Candidate: CandidateSession{ID: "second"}, Vec: []float32{0, 1}},
		{Candidate: CandidateSession{ID: "third"}, Vec: nil},
	}
	got := RerankCandidatesBySimilarity(nil, inputs, 2)
	if len(got) != 2 {
		t.Fatalf("limit not respected: got %d", len(got))
	}
	// Fallback preserves input (recency) order.
	if got[0].ID != "first" || got[1].ID != "second" {
		t.Errorf("fallback order: got %s,%s want first,second", got[0].ID, got[1].ID)
	}
}

func TestRerankCandidatesBySimilarity_FallbackWhenNoEmbeddings(t *testing.T) {
	t.Parallel()
	inputs := []CandidateRerankInput{
		{Candidate: CandidateSession{ID: "a"}},
		{Candidate: CandidateSession{ID: "b"}},
	}
	got := RerankCandidatesBySimilarity([]float32{1, 0}, inputs, 0)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("expected input order, got %+v", got)
	}
}

func TestSemanticSearch_RejectsZeroQueryVector(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	_, err := SemanticSearch(context.Background(), s.DB(), SemanticSearchOpts{
		QueryVec: []float32{0, 0},
		Model:    "m",
		Dim:      2,
		TopK:     1,
	})
	if err == nil {
		t.Error("expected error on zero query vector")
	}
}

func TestSemanticSearch_DimMismatch(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	_, err := SemanticSearch(context.Background(), s.DB(), SemanticSearchOpts{
		QueryVec: []float32{1, 0, 0},
		Model:    "m",
		Dim:      2, // mismatch
		TopK:     1,
	})
	if err == nil {
		t.Error("expected error on dim mismatch")
	}
}

func TestCosineNormed_HandlesEdgeCases(t *testing.T) {
	t.Parallel()
	if v := cosineNormed([]float32{1, 0}, []float32{0, 0}, 1); v != 0 {
		t.Errorf("zero b vec: got %v want 0", v)
	}
	if v := cosineNormed([]float32{1, 0}, []float32{1, 0}, 0); v != 0 {
		t.Errorf("zero aNorm: got %v want 0", v)
	}
	if v := cosineNormed([]float32{1, 0}, []float32{1, 0, 0}, 1); v != 0 {
		t.Errorf("len mismatch: got %v want 0", v)
	}
}
