package wire

// ScrubRequest is the body shape for POST /v1/scrub.
//
// DryRun is required: true means "scan and report, do not mutate",
// false means "rewrite for real". A missing field (or an empty body)
// is a 400 — the endpoint never infers either mode, because a bool's
// zero value would silently pick the irreversible one.
type ScrubRequest struct {
	DryRun *bool `json:"dry_run"`
}

// ScrubResponse mirrors store.ScrubReport on the wire.
type ScrubResponse struct {
	EventsScanned       int `json:"events_scanned"`
	EventsRewritten     int `json:"events_rewritten"`
	EnvelopesRewritten  int `json:"envelopes_rewritten"`
	LLMOutputsScanned   int `json:"llm_outputs_scanned"`
	LLMOutputsRewritten int `json:"llm_outputs_rewritten"`

	// ExtractionsScanned/Rewritten and SessionsRederived report the
	// coverage beyond raw_envelopes + events.content_text. Surfaced
	// because an operator has to be able to verify what a scrub
	// actually reached — reporting only the two original tables is
	// how a partial scrub previously read as a complete one.
	ExtractionsScanned   int `json:"extractions_scanned"`
	ExtractionsRewritten int `json:"extractions_rewritten"`
	SessionsRederived    int `json:"sessions_rederived"`

	PatternHits map[string]int `json:"pattern_hits"`
	DryRun      bool           `json:"dry_run"`
}

// PruneRequest is the body shape for POST /v1/prune.
//
// CutoffMs is the upper bound: sessions whose ended_at_ms is
// strictly less than it are pruned. It must be positive and not in
// the future — either extreme would prune every ended session.
// Active sessions (ended_at NULL) are always protected.
// IncludeLLMOutputs extends the prune to the LLM-output cache;
// default behaviour preserves it because summaries / reflections are
// expensive to regenerate. DryRun is required, for the same reason as
// ScrubRequest.DryRun.
type PruneRequest struct {
	CutoffMs          int64 `json:"cutoff_ms"`
	IncludeLLMOutputs bool  `json:"include_llm_outputs"`
	DryRun            *bool `json:"dry_run"`
}

// PruneResponse mirrors store.PruneReport on the wire.
type PruneResponse struct {
	Sessions     int   `json:"sessions"`
	RawEnvelopes int   `json:"raw_envelopes"`
	Events       int   `json:"events"`
	Extractions  int   `json:"extractions"`
	LLMOutputs   int   `json:"llm_outputs"`
	DryRun       bool  `json:"dry_run"`
	CutoffMs     int64 `json:"cutoff_ms"`
}
