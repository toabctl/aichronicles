package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
)

// EncodeFloat32Vec packs a float32 vector into the little-endian BLOB
// representation stored in event_embeddings.vec. dim*4 bytes by
// construction. Used by the embedder before INSERT and by tests; the
// inverse is DecodeFloat32Vec.
func EncodeFloat32Vec(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

// DecodeFloat32Vec is the inverse of EncodeFloat32Vec. Returns
// ErrBadEmbeddingBlob when len(blob) is not a multiple of 4 — that
// would indicate corruption rather than a recoverable condition.
func DecodeFloat32Vec(blob []byte) ([]float32, error) {
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("%w: length %d not a multiple of 4", ErrBadEmbeddingBlob, len(blob))
	}
	out := make([]float32, len(blob)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return out, nil
}

// ErrBadEmbeddingBlob is returned by DecodeFloat32Vec when the BLOB
// length is not a multiple of 4 bytes — indicating corruption since
// every well-formed row was packed by EncodeFloat32Vec.
var ErrBadEmbeddingBlob = errors.New("embedding blob is malformed")

// Embedding is one row of event_embeddings. Vec is the decoded
// float32 vector (the BLOB on disk is dim*4 bytes little-endian).
type Embedding struct {
	EventID     string
	Model       string
	Dim         int
	Vec         []float32
	CreatedAtMs int64
}

// SaveEmbedding inserts (or replaces) the embedding row for an event.
// Replace semantics let a re-embed with the same model overwrite an
// older vector — the dim/model columns make a mid-flight upgrade
// (text-embedding-3-small → -3-large) safe: old vectors survive
// because they have a different model value and the PK still points
// to a unique row per event.
//
// We pin the wire format here: dim*4 bytes, little-endian. Callers
// pass a []float32 and we encode; if a future caller wants to write
// raw bytes from elsewhere they can use the BLOB column directly.
func SaveEmbedding(ctx context.Context, db *sql.DB, e Embedding) error {
	if e.EventID == "" {
		return errors.New("SaveEmbedding: event_id is required")
	}
	if e.Model == "" {
		return errors.New("SaveEmbedding: model is required")
	}
	if e.Dim <= 0 {
		return errors.New("SaveEmbedding: dim must be > 0")
	}
	if len(e.Vec) != e.Dim {
		return fmt.Errorf("SaveEmbedding: vec length %d != dim %d", len(e.Vec), e.Dim)
	}
	blob := EncodeFloat32Vec(e.Vec)
	_, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO event_embeddings(event_id, model, dim, vec, created_at_ms)
		 VALUES (?, ?, ?, ?, ?)`,
		e.EventID, e.Model, e.Dim, blob, e.CreatedAtMs,
	)
	if err != nil {
		return fmt.Errorf("insert embedding for %s: %w", e.EventID, err)
	}
	return nil
}

// EmbeddingCandidate is one event that needs embedding. content_text
// is what the embedder hashes against; it's nullable in the schema
// but we filter NULL/empty out at query time so the caller never has
// to deal with it.
type EmbeddingCandidate struct {
	EventID     string
	SessionID   string
	Kind        string
	ContentText string
	TsSourceMs  int64
}

// EmbeddingCandidateFilter narrows ListEventsWithoutEmbedding. Empty
// fields disable each filter independently.
type EmbeddingCandidateFilter struct {
	// Model: an event is "missing" only if no row exists for this
	// model. Required — passing empty embeds against zero rows.
	Model string

	// SinceMs: ignore events older than this. Zero means "no cutoff".
	SinceMs int64

	// Kinds, when non-empty, limits to events with kind IN (...).
	// Useful for the embedder to skip kinds that aren't worth
	// embedding (heartbeat, transient hook noise).
	Kinds []string

	// Limit caps the result set. Non-positive uses
	// DefaultEmbeddingCandidateLimit.
	Limit int
}

// DefaultEmbeddingCandidateLimit caps the result of
// ListEventsWithoutEmbedding when the caller doesn't pass a tighter
// bound. 500 is a comfortable batch-size for the OpenAI embeddings
// endpoint (which accepts up to ~2048 inputs but charges per token —
// smaller batches keep the recovery story simple if a single call
// fails midway).
const DefaultEmbeddingCandidateLimit = 500

// ListEventsWithoutEmbedding returns events that have no
// event_embeddings row for the requested model. Excludes events with
// NULL/empty content_text (no text → nothing to embed).
//
// Order is oldest-first so a long-running embed run reaches the back
// of the backlog deterministically; callers that care about freshness
// can pass SinceMs.
func ListEventsWithoutEmbedding(ctx context.Context, db *sql.DB, filter EmbeddingCandidateFilter) ([]EmbeddingCandidate, error) {
	if filter.Model == "" {
		return nil, errors.New("ListEventsWithoutEmbedding: model is required")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultEmbeddingCandidateLimit
	}
	args := []any{filter.Model}
	q := `SELECT e.event_id, e.session_id, e.kind, e.content_text, e.ts_source_ms
		FROM events e
		LEFT JOIN event_embeddings ee
			ON ee.event_id = e.event_id AND ee.model = ?
		WHERE ee.event_id IS NULL
		  AND e.content_text IS NOT NULL
		  AND e.content_text <> ''`
	if filter.SinceMs > 0 {
		q += " AND e.ts_source_ms >= ?"
		args = append(args, filter.SinceMs)
	}
	if len(filter.Kinds) > 0 {
		placeholders := strings.Repeat(",?", len(filter.Kinds))[1:]
		for _, k := range filter.Kinds {
			args = append(args, k)
		}
		q += " AND e.kind IN (" + placeholders + ")"
	}
	q += " ORDER BY e.ts_source_ms ASC LIMIT ?"
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list embedding candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []EmbeddingCandidate
	for rows.Next() {
		var c EmbeddingCandidate
		var content sql.NullString
		if err := rows.Scan(&c.EventID, &c.SessionID, &c.Kind, &content, &c.TsSourceMs); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		c.ContentText = content.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountMissingEmbeddings reports how many events still need an
// embedding for the given model under the same filters as
// ListEventsWithoutEmbedding. Used by the `aichronicles embed`
// progress line so the user knows the size of the backlog before
// the run starts. Limit on the filter is ignored here.
func CountMissingEmbeddings(ctx context.Context, db *sql.DB, filter EmbeddingCandidateFilter) (int, error) {
	if filter.Model == "" {
		return 0, errors.New("CountMissingEmbeddings: model is required")
	}
	args := []any{filter.Model}
	q := `SELECT COUNT(*)
		FROM events e
		LEFT JOIN event_embeddings ee
			ON ee.event_id = e.event_id AND ee.model = ?
		WHERE ee.event_id IS NULL
		  AND e.content_text IS NOT NULL
		  AND e.content_text <> ''`
	if filter.SinceMs > 0 {
		q += " AND e.ts_source_ms >= ?"
		args = append(args, filter.SinceMs)
	}
	if len(filter.Kinds) > 0 {
		placeholders := strings.Repeat(",?", len(filter.Kinds))[1:]
		for _, k := range filter.Kinds {
			args = append(args, k)
		}
		q += " AND e.kind IN (" + placeholders + ")"
	}
	var n int
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count missing embeddings: %w", err)
	}
	return n, nil
}

// EmbeddingHit is one row returned by SemanticSearch. Score is cosine
// similarity in [-1, 1]; higher is more similar. Snippet is the head
// of content_text rune-truncated for display, mirroring SearchEventHit.
type EmbeddingHit struct {
	EventID    string
	SessionID  string
	Kind       string
	Cwd        sql.NullString
	TsSourceMs int64
	Content    sql.NullString
	Score      float32
}

// SemanticSearchOpts narrows the candidate pool SemanticSearch
// considers. Filter shape mirrors SearchEventOpts deliberately — once
// hybrid (FTS + cosine) lands the two paths share a planner.
type SemanticSearchOpts struct {
	// QueryVec is the embedded query. Length must equal Dim.
	QueryVec []float32

	// Model: only embeddings produced by this model are scored.
	// Required so cross-model comparisons (incompatible vector
	// spaces) can't silently produce garbage results.
	Model string

	// Dim: the dimensionality of QueryVec. Used as a sanity check
	// against on-disk rows; mismatches are skipped.
	Dim int

	// Optional facet filters (mirror SearchEventOpts).
	SessionID   string
	SourceAgent string
	Kind        string
	SinceMs     int64

	// TopK is the number of hits returned, sorted by score DESC.
	// Non-positive uses 20.
	TopK int
}

// SemanticSearch loads every stored embedding matching the filters,
// computes cosine similarity against QueryVec in Go, and returns the
// top-K hits.
//
// O(N) on the number of stored rows for the configured model — no
// index. Acceptable up to roughly 100k rows on commodity hardware
// (~50ms per query); past that point we'd need an ANN index, but
// upgrading the SQLite driver to one that supports sqlite-vec is a
// bigger blast radius than this feature warrants today.
func SemanticSearch(ctx context.Context, db *sql.DB, opts SemanticSearchOpts) ([]EmbeddingHit, error) {
	if opts.Model == "" {
		return nil, errors.New("SemanticSearch: model is required")
	}
	if opts.Dim <= 0 {
		return nil, errors.New("SemanticSearch: dim must be > 0")
	}
	if len(opts.QueryVec) != opts.Dim {
		return nil, fmt.Errorf("SemanticSearch: query length %d != dim %d", len(opts.QueryVec), opts.Dim)
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = 20
	}

	args := []any{opts.Model, opts.Dim}
	q := `SELECT ee.event_id, e.session_id, e.kind, e.cwd, e.ts_source_ms,
			e.content_text, ee.vec
		FROM event_embeddings ee
		JOIN events e ON e.event_id = ee.event_id
		WHERE ee.model = ? AND ee.dim = ?`
	if opts.SessionID != "" {
		q += " AND e.session_id = ?"
		args = append(args, opts.SessionID)
	}
	if opts.SourceAgent != "" {
		q += " AND e.source_agent = ?"
		args = append(args, opts.SourceAgent)
	}
	if opts.Kind != "" {
		q += " AND e.kind = ?"
		args = append(args, opts.Kind)
	}
	if opts.SinceMs > 0 {
		q += " AND e.ts_source_ms >= ?"
		args = append(args, opts.SinceMs)
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	queryNorm := vecNorm(opts.QueryVec)
	if queryNorm == 0 {
		return nil, errors.New("SemanticSearch: query vector is zero")
	}

	// Stream-and-prune: keep only the top-K best so we don't hold
	// every row in RAM. At topK ~ 20 a linear scan beats a heap.
	hits := make([]EmbeddingHit, 0, topK+1)
	worst := float32(-2) // cosine in [-1,1]; sentinel below the floor

	for rows.Next() {
		var (
			h    EmbeddingHit
			blob []byte
		)
		if err := rows.Scan(&h.EventID, &h.SessionID, &h.Kind, &h.Cwd,
			&h.TsSourceMs, &h.Content, &blob); err != nil {
			return nil, fmt.Errorf("semantic scan: %w", err)
		}
		v, err := DecodeFloat32Vec(blob)
		if err != nil || len(v) != opts.Dim {
			// Skip malformed / mismatched-dim rows rather than
			// failing the whole query. The dim check at the SQL
			// level already eliminates the common case.
			continue
		}
		score := cosineNormed(opts.QueryVec, v, queryNorm)
		if len(hits) < topK {
			h.Score = score
			hits = insertSortedDesc(hits, h)
			if len(hits) == topK {
				worst = hits[len(hits)-1].Score
			}
			continue
		}
		if score > worst {
			h.Score = score
			hits = hits[:len(hits)-1]
			hits = insertSortedDesc(hits, h)
			worst = hits[len(hits)-1].Score
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("semantic iterate: %w", err)
	}
	return hits, nil
}

// insertSortedDesc inserts hit into a slice already sorted by Score
// DESC, returning the resulting slice. Linear; fine for topK ~ 20.
func insertSortedDesc(hits []EmbeddingHit, hit EmbeddingHit) []EmbeddingHit {
	idx := len(hits)
	for i, h := range hits {
		if hit.Score > h.Score {
			idx = i
			break
		}
	}
	hits = append(hits, EmbeddingHit{})
	copy(hits[idx+1:], hits[idx:])
	hits[idx] = hit
	return hits
}

// vecNorm returns the L2 norm of v.
func vecNorm(v []float32) float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return float32(math.Sqrt(sum))
}

// cosineNormed computes cosine similarity between a (with precomputed
// L2 norm aNorm) and b. Returns 0 when either norm is zero — a
// degenerate but finite answer beats NaN propagating through ranking.
func cosineNormed(a, b []float32, aNorm float32) float32 {
	if len(a) != len(b) || aNorm == 0 {
		return 0
	}
	var dot, bSum float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		bSum += float64(b[i]) * float64(b[i])
	}
	bNorm := math.Sqrt(bSum)
	if bNorm == 0 {
		return 0
	}
	return float32(dot / (float64(aNorm) * bNorm))
}
