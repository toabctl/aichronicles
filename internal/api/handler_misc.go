package api

import (
	"net/http"
	"strconv"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// handleSessionLLMOutputs serves
// GET /v1/sessions/{id}/llm-outputs?kind=&limit=. Returns every
// llm_outputs row for the session, optionally filtered by kind.
// Used by MCP get_summary (when kind != summary) and the
// summarize CLI for cache hit-rate inspection.
func (s *Server) handleSessionLLMOutputs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "Missing session id", "")
		return
	}
	kind := r.URL.Query().Get("kind")
	limit, ok := parseLimitQuery(w, r, 50)
	if !ok {
		return
	}
	rows, err := store.LoadLLMOutputsForSession(r.Context(), s.store.DB(), id)
	if err != nil {
		s.storeError(w, "LoadLLMOutputsForSession", err)
		return
	}
	out := make([]wire.LLMOutput, 0, len(rows))
	for _, o := range rows {
		if kind != "" && string(o.Kind) != kind {
			continue
		}
		out = append(out, llmOutputToWire(o))
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, wire.LLMOutputsListResponse{Outputs: out})
}

// handleLLMOutputsList serves
// GET /v1/llm-outputs/list?kind=&session_id=&since_ms=&limit=.
// Filtered list of LLM outputs across sessions; used by MCP
// list_workflows (kind=induction) and the digest CLI.
func (s *Server) handleLLMOutputsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.LLMOutputFilter{
		Kind:      store.LLMOutputKind(q.Get("kind")),
		SessionID: q.Get("session_id"),
	}
	limit, ok := parseLimitQuery(w, r, 50)
	if !ok {
		return
	}
	filter.Limit = limit

	rows, err := store.LoadLLMOutputs(r.Context(), s.store.DB(), filter)
	if err != nil {
		s.storeError(w, "LoadLLMOutputs", err)
		return
	}
	out := make([]wire.LLMOutput, 0, len(rows))
	for _, o := range rows {
		out = append(out, llmOutputToWire(o))
	}
	writeJSON(w, http.StatusOK, wire.LLMOutputsListResponse{Outputs: out})
}

// handleSummariesGet serves GET /v1/summaries?session_id=<id>.
// Returns 404 when the session has no summary yet (collection
// vs resource: a summary is a per-session resource).
func (s *Server) handleSummariesGet(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeProblem(w, http.StatusBadRequest, "Missing session_id", "")
		return
	}
	rows, err := store.LoadLLMOutputsForSession(r.Context(), s.store.DB(), sessionID)
	if err != nil {
		s.storeError(w, "LoadLLMOutputsForSession", err)
		return
	}
	for _, o := range rows {
		if o.Kind == store.LLMKindSummary {
			writeJSON(w, http.StatusOK, llmOutputToWire(o))
			return
		}
	}
	writeProblem(w, http.StatusNotFound, "No summary for session", sessionID)
}

// handleSummariesBatch serves GET /v1/summaries/batch?session_ids=id1,id2,id3.
// Returns a {session_id → LLMOutput} map for sessions that have a
// cached summary; sessions without one are omitted from the map.
// One round-trip replacing N /v1/summaries?session_id= calls — the
// web's session-list page is the primary consumer.
func (s *Server) handleSummariesBatch(w http.ResponseWriter, r *http.Request) {
	ids := parseSessionIDsQuery(r.URL.Query().Get("session_ids"))
	if len(ids) == 0 {
		writeProblem(w, http.StatusBadRequest, "Missing session_ids",
			"session_ids is a comma-separated list of ids")
		return
	}
	rows, err := store.LoadSummariesIndexedByID(r.Context(), s.store.DB(), ids)
	if err != nil {
		s.storeError(w, "LoadSummariesIndexedByID", err)
		return
	}
	out := wire.SummariesBatchResponse{Summaries: make(map[string]wire.LLMOutput, len(rows))}
	for id, row := range rows {
		out.Summaries[id] = llmOutputToWire(row)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleLLMOutputGet serves GET /v1/llm-outputs?kind=&prompt_hash=.
// Used by callers that want to check the LLM-output cache by hash
// before paying for a regeneration.
func (s *Server) handleLLMOutputGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	hash := q.Get("prompt_hash")
	if kind == "" || hash == "" {
		writeProblem(w, http.StatusBadRequest, "Missing kind or prompt_hash", "")
		return
	}
	row, err := store.LoadLLMOutputByHash(r.Context(), s.store.DB(), store.LLMOutputKind(kind), hash)
	if err != nil {
		s.storeError(w, "LoadLLMOutputByHash", err)
		return
	}
	if row == nil {
		writeProblem(w, http.StatusNotFound, "LLM output not found", "")
		return
	}
	writeJSON(w, http.StatusOK, llmOutputToWire(*row))
}

// handleUnresolvedForCwd serves GET /v1/unresolved?cwd=&since_ms=
// &max_sessions=&max_items_per_session=. cwd is required.
func (s *Server) handleUnresolvedForCwd(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cwd := q.Get("cwd")
	if cwd == "" {
		writeProblem(w, http.StatusBadRequest, "Missing cwd", "")
		return
	}
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	maxSess, ok := parseNonNegativeIntQuery(w, r, "max_sessions", 0)
	if !ok {
		return
	}
	maxItems, ok := parseNonNegativeIntQuery(w, r, "max_items_per_session", 0)
	if !ok {
		return
	}
	rows, err := store.LoadUnresolvedForCwd(r.Context(), s.store.DB(), cwd, sinceMs, maxSess, maxItems)
	if err != nil {
		s.storeError(w, "LoadUnresolvedForCwd", err)
		return
	}
	out := wire.UnresolvedResponse{Items: make([]wire.UnresolvedItem, 0, len(rows))}
	for _, it := range rows {
		out.Items = append(out.Items, wire.UnresolvedItem{
			SessionID:    it.SessionID,
			SessionShort: it.SessionShort,
			EndedAtMs:    it.EndedAtMs,
			Topic:        it.Topic,
			Item:         it.Item,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleProjectsAggregates serves GET /v1/projects/aggregates?since_ms=.
func (s *Server) handleProjectsAggregates(w http.ResponseWriter, r *http.Request) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	rows, err := store.LoadProjectAggregates(r.Context(), s.store.DB(), sinceMs)
	if err != nil {
		s.storeError(w, "LoadProjectAggregates", err)
		return
	}
	out := wire.ProjectAggregatesResponse{Projects: make([]wire.ProjectAggregate, 0, len(rows))}
	for _, p := range rows {
		out.Projects = append(out.Projects, wire.ProjectAggregate{
			Cwd:            p.Cwd,
			Sessions:       p.Sessions,
			Events:         p.Events,
			LastActivityMs: p.LastActivityMs,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSubagentSpans serves GET /v1/subagents?session_id=&limit=.
// Empty session_id returns spans across the whole store.
func (s *Server) handleSubagentSpans(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	limit, ok := parseLimitQuery(w, r, 50)
	if !ok {
		return
	}
	rows, err := store.LoadSubagentSpans(r.Context(), s.store.DB(), sessionID, limit)
	if err != nil {
		s.storeError(w, "LoadSubagentSpans", err)
		return
	}
	out := wire.SubagentsResponse{Spans: make([]wire.SubagentSpan, 0, len(rows))}
	for _, sp := range rows {
		out.Spans = append(out.Spans, wire.SubagentSpan{
			SessionID:    sp.SessionID,
			SubagentID:   sp.SubagentID,
			SubagentType: sp.SubagentType,
			StartedAtMs:  sp.StartedAtMs,
			EndedAtMs:    sp.EndedAtMs,
			EventCount:   sp.EventCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleInsights serves GET /v1/insights?since_ms=&top_tools=&top_skills=&top_sessions=.
func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	lim := store.InsightsLimits{}
	if lim.TopTools, ok = parsePositiveIntQuery(w, r, "top_tools", 0); !ok {
		return
	}
	if lim.TopSkills, ok = parsePositiveIntQuery(w, r, "top_skills", 0); !ok {
		return
	}
	if lim.TopSessions, ok = parsePositiveIntQuery(w, r, "top_sessions", 0); !ok {
		return
	}
	rep, err := store.LoadInsights(r.Context(), s.store.DB(), sinceMs, lim)
	if err != nil {
		s.storeError(w, "LoadInsights", err)
		return
	}
	writeJSON(w, http.StatusOK, insightsToWire(rep))
}

// llmOutputToWire projects store.LLMOutput → wire.LLMOutput. Kind
// is the underlying string of LLMOutputKind.
func llmOutputToWire(o store.LLMOutput) wire.LLMOutput {
	return wire.LLMOutput{
		ID:           o.ID,
		SessionID:    o.SessionID,
		Kind:         string(o.Kind),
		Model:        o.Model,
		PromptHash:   o.PromptHash,
		InputTokens:  o.InputTokens,
		OutputTokens: o.OutputTokens,
		Body:         o.Body,
		CreatedAtMs:  o.CreatedAtMs,
	}
}

// insightsToWire projects store.InsightsReport → wire.Insights,
// flattening sql.NullInt64 / sql.NullString in TopSession.
func insightsToWire(r *store.InsightsReport) wire.Insights {
	if r == nil {
		return wire.Insights{}
	}
	out := wire.Insights{
		Window: wire.InsightsWindow{
			SinceMs: r.Window.SinceMs,
			UntilMs: r.Window.UntilMs,
			Days:    r.Window.Days,
		},
		Overview: wire.InsightsOverview{
			Sessions:       r.Overview.Sessions,
			Events:         r.Overview.Events,
			ToolUses:       r.Overview.ToolUses,
			UserPrompts:    r.Overview.UserPrompts,
			DistinctTools:  r.Overview.DistinctTools,
			DistinctSkills: r.Overview.DistinctSkills,
		},
		TopTools:       make([]wire.ToolUsage, 0, len(r.TopTools)),
		TopSkills:      make([]wire.SkillUsage, 0, len(r.TopSkills)),
		ActivityByHour: make([]wire.HourBucket, 0, len(r.ActivityByHour)),
		TopSessions:    make([]wire.TopSession, 0, len(r.TopSessions)),
	}
	for _, t := range r.TopTools {
		out.TopTools = append(out.TopTools, wire.ToolUsage{ToolName: t.ToolName, Count: t.Count})
	}
	for _, sk := range r.TopSkills {
		out.TopSkills = append(out.TopSkills, wire.SkillUsage{
			Name: sk.Name, Count: sk.Count, LastUsedMs: sk.LastUsedMs,
		})
	}
	for _, h := range r.ActivityByHour {
		out.ActivityByHour = append(out.ActivityByHour, wire.HourBucket{Hour: h.Hour, Count: h.Count})
	}
	for _, ts := range r.TopSessions {
		out.TopSessions = append(out.TopSessions, wire.TopSession{
			SessionID:   ts.SessionID,
			EventCount:  ts.EventCount,
			StartedAtMs: ts.StartedAtMs,
			EndedAtMs:   ts.EndedAtMs,
			Cwd:         ts.Cwd,
			FirstPrompt: ts.FirstPrompt,
		})
	}
	return out
}

// handleLLMOutputByID serves GET /v1/llm-outputs/{id}. Returns the
// row by primary key or 404. Used by reflect / propose / facts /
// induction to re-load the row they just persisted (the cached
// caller doesn't ship the whole body to runCachedLLM's hooks).
func (s *Server) handleLLMOutputByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid id", "")
		return
	}
	row, err := store.LoadLLMOutputByID(r.Context(), s.store.DB(), id)
	if err != nil {
		s.storeError(w, "LoadLLMOutputByID", err)
		return
	}
	if row == nil {
		writeProblem(w, http.StatusNotFound, "LLM output not found", "")
		return
	}
	writeJSON(w, http.StatusOK, llmOutputToWire(*row))
}

// handleLLMOutputsLastCreated serves
// GET /v1/llm-outputs/last-created-at?kind=. Used by the meta
// sweeper's cadence gate — last fired-at per kind.
func (s *Server) handleLLMOutputsLastCreated(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		writeProblem(w, http.StatusBadRequest, "Missing kind", "")
		return
	}
	last, err := store.LastLLMOutputCreatedAt(r.Context(), s.store.DB(), store.LLMOutputKind(kind))
	if err != nil {
		s.storeError(w, "LastLLMOutputCreatedAt", err)
		return
	}
	writeJSON(w, http.StatusOK, wire.LLMOutputLastCreatedAtResponse{LastCreatedAtMs: last})
}

// handleLLMOutputExistsForSession serves
// GET /v1/llm-outputs/exists?session_id=&kind=. A cheap row-exists
// probe used by the induction sweeper to decide whether phase 1
// (auto-summarize) needs to fire.
func (s *Server) handleLLMOutputExistsForSession(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sessionID := q.Get("session_id")
	kind := q.Get("kind")
	if sessionID == "" || kind == "" {
		writeProblem(w, http.StatusBadRequest, "Missing session_id or kind", "")
		return
	}
	has, err := store.HasLLMOutputForSession(r.Context(), s.store.DB(), sessionID, store.LLMOutputKind(kind))
	if err != nil {
		s.storeError(w, "HasLLMOutputForSession", err)
		return
	}
	writeJSON(w, http.StatusOK, wire.LLMOutputExistsResponse{Exists: has})
}
