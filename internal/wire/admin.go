package wire

// ScrubRequest is the body shape for POST /v1/scrub.
//
// DryRun=true (the default semantics on a freshly-decoded zero
// value) means "scan and report, do not mutate." Clients that
// genuinely want to rewrite must send DryRun=false explicitly —
// the endpoint never accepts an empty body as "go ahead and
// scrub everything live."
type ScrubRequest struct {
	DryRun bool `json:"dry_run"`
}

// ScrubResponse mirrors store.ScrubReport on the wire.
type ScrubResponse struct {
	EventsScanned       int            `json:"events_scanned"`
	EventsRewritten     int            `json:"events_rewritten"`
	EnvelopesRewritten  int            `json:"envelopes_rewritten"`
	LLMOutputsScanned   int            `json:"llm_outputs_scanned"`
	LLMOutputsRewritten int            `json:"llm_outputs_rewritten"`
	PatternHits         map[string]int `json:"pattern_hits"`
	DryRun              bool           `json:"dry_run"`
}

// PruneRequest is the body shape for POST /v1/prune.
//
// CutoffMs is the lower bound: rows whose ended_at_ms is strictly
// less than this are pruned. Active sessions (ended_at NULL) are
// always protected. IncludeLLMOutputs extends the prune to the
// LLM-output cache; default behaviour preserves it because
// summaries / reflections are expensive to regenerate.
type PruneRequest struct {
	CutoffMs          int64 `json:"cutoff_ms"`
	IncludeLLMOutputs bool  `json:"include_llm_outputs"`
	DryRun            bool  `json:"dry_run"`
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
