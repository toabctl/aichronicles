package api

import (
	"net/http"
	"strconv"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/api"
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
	limit := parseLimit(r, 50)
	if limit < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
		return
	}
	rows, err := store.LoadLLMOutputsForSession(r.Context(), s.store.DB(), id)
	if err != nil {
		s.slog.Error("LoadLLMOutputsForSession", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := make([]api.LLMOutput, 0, len(rows))
	for _, o := range rows {
		if kind != "" && string(o.Kind) != kind {
			continue
		}
		out = append(out, llmOutputToWire(o))
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Outputs []api.LLMOutput `json:"outputs"`
	}{Outputs: out})
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
	limit := parseLimit(r, 50)
	if limit < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
		return
	}
	filter.Limit = limit

	rows, err := store.LoadLLMOutputs(r.Context(), s.store.DB(), filter)
	if err != nil {
		s.slog.Error("LoadLLMOutputs", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := make([]api.LLMOutput, 0, len(rows))
	for _, o := range rows {
		out = append(out, llmOutputToWire(o))
	}
	writeJSON(w, http.StatusOK, struct {
		Outputs []api.LLMOutput `json:"outputs"`
	}{Outputs: out})
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
		s.slog.Error("LoadLLMOutputsForSession", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
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
		s.slog.Error("LoadLLMOutputByHash", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
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
	maxSess := positiveOrZero(q.Get("max_sessions"))
	if maxSess < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid max_sessions", "")
		return
	}
	maxItems := positiveOrZero(q.Get("max_items_per_session"))
	if maxItems < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid max_items_per_session", "")
		return
	}
	rows, err := store.LoadUnresolvedForCwd(r.Context(), s.store.DB(), cwd, sinceMs, maxSess, maxItems)
	if err != nil {
		s.slog.Error("LoadUnresolvedForCwd", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := api.UnresolvedResponse{Items: make([]api.UnresolvedItem, 0, len(rows))}
	for _, it := range rows {
		out.Items = append(out.Items, api.UnresolvedItem{
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
		s.slog.Error("LoadProjectAggregates", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := api.ProjectAggregatesResponse{Projects: make([]api.ProjectAggregate, 0, len(rows))}
	for _, p := range rows {
		out.Projects = append(out.Projects, api.ProjectAggregate{
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
	limit := parseLimit(r, 50)
	if limit < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
		return
	}
	rows, err := store.LoadSubagentSpans(r.Context(), s.store.DB(), sessionID, limit)
	if err != nil {
		s.slog.Error("LoadSubagentSpans", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := api.SubagentsResponse{Spans: make([]api.SubagentSpan, 0, len(rows))}
	for _, sp := range rows {
		out.Spans = append(out.Spans, api.SubagentSpan{
			SessionID:    sp.SessionID,
			SubagentID:   sp.SubagentID,
			SubagentType: sqlNullToPtr(sp.SubagentType),
			StartedAtMs:  sp.StartedAtMs,
			EndedAtMs:    sp.EndedAtMs,
			EventCount:   sp.EventCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleInsights serves GET /v1/insights?since_ms=&top_tools=&top_skills=&top_sessions=.
func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	lim := store.InsightsLimits{}
	if v := q.Get("top_tools"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid top_tools", "")
			return
		}
		lim.TopTools = n
	}
	if v := q.Get("top_skills"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid top_skills", "")
			return
		}
		lim.TopSkills = n
	}
	if v := q.Get("top_sessions"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid top_sessions", "")
			return
		}
		lim.TopSessions = n
	}
	rep, err := store.LoadInsights(r.Context(), s.store.DB(), sinceMs, lim)
	if err != nil {
		s.slog.Error("LoadInsights", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	writeJSON(w, http.StatusOK, insightsToWire(rep))
}

// llmOutputToWire projects store.LLMOutput → api.LLMOutput. Kind
// is the underlying string of LLMOutputKind.
func llmOutputToWire(o store.LLMOutput) api.LLMOutput {
	return api.LLMOutput{
		ID:           o.ID,
		SessionID:    sqlNullToPtr(o.SessionID),
		Kind:         string(o.Kind),
		Model:        o.Model,
		PromptHash:   o.PromptHash,
		InputTokens:  sqlNullInt64ToPtr(o.InputTokens),
		OutputTokens: sqlNullInt64ToPtr(o.OutputTokens),
		Body:         o.Body,
		CreatedAtMs:  o.CreatedAtMs,
	}
}

// insightsToWire projects store.InsightsReport → api.Insights,
// flattening sql.NullInt64 / sql.NullString in TopSession.
func insightsToWire(r *store.InsightsReport) api.Insights {
	if r == nil {
		return api.Insights{}
	}
	out := api.Insights{
		Window: api.InsightsWindow{
			SinceMs: r.Window.SinceMs,
			UntilMs: r.Window.UntilMs,
			Days:    r.Window.Days,
		},
		Overview: api.InsightsOverview{
			Sessions:       r.Overview.Sessions,
			Events:         r.Overview.Events,
			ToolUses:       r.Overview.ToolUses,
			UserPrompts:    r.Overview.UserPrompts,
			DistinctTools:  r.Overview.DistinctTools,
			DistinctSkills: r.Overview.DistinctSkills,
		},
		TopTools:       make([]api.ToolUsage, 0, len(r.TopTools)),
		TopSkills:      make([]api.SkillUsage, 0, len(r.TopSkills)),
		ActivityByHour: make([]api.HourBucket, 0, len(r.ActivityByHour)),
		TopSessions:    make([]api.TopSession, 0, len(r.TopSessions)),
	}
	for _, t := range r.TopTools {
		out.TopTools = append(out.TopTools, api.ToolUsage{ToolName: t.ToolName, Count: t.Count})
	}
	for _, sk := range r.TopSkills {
		out.TopSkills = append(out.TopSkills, api.SkillUsage{
			Name: sk.Name, Count: sk.Count, LastUsedMs: sk.LastUsedMs,
		})
	}
	for _, h := range r.ActivityByHour {
		out.ActivityByHour = append(out.ActivityByHour, api.HourBucket{Hour: h.Hour, Count: h.Count})
	}
	for _, ts := range r.TopSessions {
		out.TopSessions = append(out.TopSessions, api.TopSession{
			SessionID:   ts.SessionID,
			EventCount:  ts.EventCount,
			StartedAtMs: sqlNullInt64ToPtr(ts.StartedAtMs),
			EndedAtMs:   sqlNullInt64ToPtr(ts.EndedAtMs),
			Cwd:         sqlNullToPtr(ts.Cwd),
			FirstPrompt: ts.FirstPrompt,
		})
	}
	return out
}

// positiveOrZero parses "" → 0, valid positive int → that int,
// otherwise -1 to signal a 400. Used by handlers that have
// optional non-negative limits without the parseLimit default
// behavior.
func positiveOrZero(v string) int {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return -1
	}
	return n
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
		s.slog.Error("LoadLLMOutputByID", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
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
		s.slog.Error("LastLLMOutputCreatedAt", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		LastCreatedAtMs int64 `json:"last_created_at_ms"`
	}{LastCreatedAtMs: last})
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
		s.slog.Error("HasLLMOutputForSession", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Exists bool `json:"exists"`
	}{Exists: has})
}
