package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/timefmt"
	"github.com/toabctl/aichronicles/internal/wire"
)

// workflowsHandler renders /workflows — every kind=induction row
// whose body.workflow is non-null. After Round 8 workflows live
// inside induction LLM-output bodies, not in their own kind, so
// this handler reads kind=induction and filters Go-side.
//
// Sessions where the LLM emitted no workflow (the common case) are
// elided. Pre-renders procedure / preconditions / success_checks
// as flat slices so the template stays free of nested lookups.
//
// Every row is read (across pages): the page used to read the newest
// 200 induction rows and silently showed the workflows among them —
// 134 of 269 on a real store — under a heading that counts them as
// the whole corpus.
func (s *Server) workflowsHandler(w http.ResponseWriter, r *http.Request) {
	page := WorkflowsPage{Title: "Workflows"}
	now := time.Now().UTC()
	for r, err := range s.api.LLMOutputs(r.Context(), string(wire.LLMKindInduction), "") {
		if err != nil {
			s.internalError(w, "workflowsHandler: load induction rows", "internal error", err)
			return
		}
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
		if r.SessionID != nil {
			row.SessionID = *r.SessionID
			row.SessionShort = preview.ShortID(*r.SessionID)
		}
		for _, step := range ind.Workflow.Procedure {
			row.Procedure = append(row.Procedure, step.Action)
		}
		page.Workflows = append(page.Workflows, row)
	}
	s.render(w, r, "workflows", page)
}
