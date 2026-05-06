package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/timefmt"
)

// workflowsHandler renders /workflows — every kind=induction row
// whose body.workflow is non-null. After Round 8 workflows live
// inside induction LLM-output bodies, not in their own kind, so
// this handler reads kind=induction and filters Go-side.
//
// Sessions where the LLM emitted no workflow (the common case) are
// elided. Pre-renders procedure / preconditions / success_checks
// as flat slices so the template stays free of nested lookups.
func (s *Server) workflowsHandler(w http.ResponseWriter, r *http.Request) {
	const corpusCap = 200
	rows, err := store.LoadLLMOutputs(r.Context(), s.store.DB(), store.LLMOutputFilter{
		Kind:  store.LLMKindInduction,
		Limit: corpusCap,
	})
	if err != nil {
		s.log.Error("workflowsHandler: load induction rows", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	page := WorkflowsPage{Title: "Workflows"}
	now := time.Now().UTC()
	for _, r := range rows {
		var ind prompts.InductionResult
		if jerr := json.Unmarshal([]byte(r.Body), &ind); jerr != nil {
			continue
		}
		if ind.Workflow == nil {
			continue
		}
		row := WorkflowRow{
			TaskShape:     ind.Workflow.TaskShape,
			InducedAgo:    timefmt.Relative(r.CreatedAtMs, now),
			Preconditions: ind.Workflow.Preconditions,
			SuccessChecks: ind.Workflow.SuccessChecks,
		}
		if r.SessionID.Valid {
			row.SessionID = r.SessionID.String
			row.SessionShort = shortID(r.SessionID.String)
		}
		for _, step := range ind.Workflow.Procedure {
			row.Procedure = append(row.Procedure, step.Action)
		}
		page.Workflows = append(page.Workflows, row)
	}
	s.render(w, r, "workflows", page)
}
