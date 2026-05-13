package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/toabctl/aichronicles/internal/nullable"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// handleSessionsMissingSummary serves
// GET /v1/sessions/missing-summary?since_ms=&cwd=&agent=&limit=.
func (s *Server) handleSessionsMissingSummary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	limit, ok := parsePositiveIntQuery(w, r, "limit", 200)
	if !ok {
		return
	}
	rows, err := store.LoadSessionsMissingSummary(r.Context(), s.store.DB(),
		sinceMs, store.SessionFilter{Cwd: q.Get("cwd"), Agent: q.Get("agent")}, limit)
	if err != nil {
		s.slog.Error("LoadSessionsMissingSummary", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := wire.SessionsMissingSummaryResponse{Sessions: make([]wire.SessionDigest, 0, len(rows))}
	for _, row := range rows {
		out.Sessions = append(out.Sessions, sessionDigestRowToWire(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSessionsNeedingSegmentation serves
// GET /v1/sessions/needing-segmentation?idle_cutoff_ms=&idle_ms=&min_events=&limit=.
func (s *Server) handleSessionsNeedingSegmentation(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	idleCutoff, ok := parseInt64Query(w, r, "idle_cutoff_ms")
	if !ok {
		return
	}
	idleMs, ok := parseInt64Query(w, r, "idle_ms")
	if !ok {
		return
	}
	minEvents := positiveOrZero(q.Get("min_events"))
	if minEvents < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid min_events", "")
		return
	}
	limit := positiveOrZero(q.Get("limit"))
	if limit < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
		return
	}
	ids, err := store.LoadSessionsNeedingSegmentation(r.Context(), s.store.DB(),
		idleCutoff, idleMs, minEvents, limit)
	if err != nil {
		s.slog.Error("LoadSessionsNeedingSegmentation", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, wire.SessionsNeedingSegmentationResponse{SessionIDs: ids})
}

// handleSessionsForCompletion serves GET /v1/sessions/completions?prefix=&limit=.
// Drives shell-completion for `--session` flags.
func (s *Server) handleSessionsForCompletion(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	limit, ok := parsePositiveIntQuery(w, r, "limit", 25)
	if !ok {
		return
	}
	rows, err := store.LoadSessionsForCompletion(r.Context(), s.store.DB(), prefix, limit)
	if err != nil {
		s.slog.Error("LoadSessionsForCompletion", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := wire.SessionCompletionsResponse{Sessions: make([]wire.SessionCompletion, 0, len(rows))}
	for _, row := range rows {
		out.Sessions = append(out.Sessions, wire.SessionCompletion{
			ID:          row.ID,
			Description: row.Description,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleInductionCandidates serves GET /v1/induction/candidates.
func (s *Server) handleInductionCandidates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nowMs, ok := parseInt64Query(w, r, "now_ms")
	if !ok {
		return
	}
	idleMs, ok := parseInt64Query(w, r, "idle_threshold_ms")
	if !ok {
		return
	}
	minEvents := positiveOrZero(q.Get("min_events"))
	if minEvents < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid min_events", "")
		return
	}
	limit := positiveOrZero(q.Get("limit"))
	if limit < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
		return
	}
	rows, err := store.LoadInductionCandidates(r.Context(), s.store.DB(),
		nowMs, idleMs, minEvents, limit)
	if err != nil {
		s.slog.Error("LoadInductionCandidates", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := wire.InductionCandidatesResponse{Candidates: make([]wire.InductionCandidate, 0, len(rows))}
	for _, row := range rows {
		out.Candidates = append(out.Candidates, wire.InductionCandidate{
			ID:          row.ID,
			EventCount:  row.EventCount,
			StartedAtMs: nullable.Int64Ptr(row.StartedAtMs),
			EndedAtMs:   nullable.Int64Ptr(row.EndedAtMs),
			Cwd:         nullable.StringPtr(row.Cwd),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFailureShapes serves GET /v1/proposals/failure-shapes.
func (s *Server) handleFailureShapes(w http.ResponseWriter, r *http.Request) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	limit := positiveOrZero(r.URL.Query().Get("limit"))
	if limit < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
		return
	}
	rows, err := store.LoadFailureShapes(r.Context(), s.store.DB(), sinceMs, limit)
	if err != nil {
		s.slog.Error("LoadFailureShapes", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := wire.FailureShapesResponse{Shapes: make([]wire.FailureShape, 0, len(rows))}
	for _, row := range rows {
		out.Shapes = append(out.Shapes, wire.FailureShape{
			SessionID:         row.SessionID,
			Title:             row.Title,
			ToolFailureCount:  row.ToolFailureCount,
			GitUndoCount:      row.GitUndoCount,
			PromptRepeatCount: row.PromptRepeatCount,
			EndedAtMs:         nullable.Int64Ptr(row.EndedAtMs),
			Cwd:               nullable.StringPtr(row.Cwd),
			LastEventKind:     nullable.StringPtr(row.LastEventKind),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSkillFailures serves GET /v1/skills/failures?skill=&since_ms=&window_ms=&limit=.
func (s *Server) handleSkillFailures(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	skill := q.Get("skill")
	if skill == "" {
		writeProblem(w, http.StatusBadRequest, "Missing skill", "")
		return
	}
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	windowMs, ok := parseInt64Query(w, r, "window_ms")
	if !ok {
		return
	}
	limit := positiveOrZero(q.Get("limit"))
	if limit < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
		return
	}
	rows, err := store.LoadSkillFailures(r.Context(), s.store.DB(), skill, sinceMs, windowMs, limit)
	if err != nil {
		s.slog.Error("LoadSkillFailures", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := wire.SkillFailuresResponse{Failures: make([]wire.SkillFailureContext, 0, len(rows))}
	for _, row := range rows {
		out.Failures = append(out.Failures, wire.SkillFailureContext{
			SessionID:  row.SessionID,
			LoadTsMs:   row.LoadTsMs,
			FailTsMs:   row.FailTsMs,
			FailBody:   row.FailBody,
			NearbyText: row.NearbyText,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSkillCandidatesEffectiveness serves
// GET /v1/skill-candidates/effectiveness?since_ms=&window_ms=&limit=.
func (s *Server) handleSkillCandidatesEffectiveness(w http.ResponseWriter, r *http.Request) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	windowMs, ok := parseInt64Query(w, r, "window_ms")
	if !ok {
		return
	}
	limit := positiveOrZero(r.URL.Query().Get("limit"))
	if limit < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
		return
	}
	rows, err := store.LoadSkillCandidateEffectiveness(r.Context(), s.store.DB(), sinceMs, windowMs, limit)
	if err != nil {
		s.slog.Error("LoadSkillCandidateEffectiveness", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := wire.SkillCandidateEffectivenessResponse{Rows: make([]wire.SkillCandidateEffectiveness, 0, len(rows))}
	for _, row := range rows {
		out.Rows = append(out.Rows, wire.SkillCandidateEffectiveness{
			CandidateID:      row.CandidateID,
			LLMOutputID:      row.LLMOutputID,
			SkillName:        row.SkillName,
			ProposedAtMs:     row.ProposedAtMs,
			AddedAtMs:        row.AddedAtMs,
			AddPath:          row.AddPath,
			LoadsAfterAdd:    row.LoadsAfterAdd,
			FailedLoadsAfter: row.FailedLoadsAfter,
			LastLoadedMs:     nullable.Int64Ptr(row.LastLoadedMs),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSkillCandidatesPending serves
// GET /v1/skill-candidates/pending?since_ms=&limit=. Used by
// propose's prior-art enrichment to surface candidates the user
// has not acted on (decision IS NULL).
func (s *Server) handleSkillCandidatesPending(w http.ResponseWriter, r *http.Request) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	limit := positiveOrZero(r.URL.Query().Get("limit"))
	if limit < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
		return
	}
	rows, err := store.LoadPendingSkillCandidates(r.Context(), s.store.DB(), sinceMs, limit)
	if err != nil {
		s.slog.Error("LoadPendingSkillCandidates", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := wire.PendingSkillCandidatesResponse{Candidates: make([]wire.SkillCandidate, 0, len(rows))}
	for _, row := range rows {
		out.Candidates = append(out.Candidates, skillCandidateRowToWire(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSkillCandidatesAdded serves
// GET /v1/skill-candidates/added?name=. Returns the most-recent
// row whose decision='add' for the named skill, or 404 when none
// exists (the skill is hand-authored — caller should treat that
// as a hand-authored merge target).
func (s *Server) handleSkillCandidatesAdded(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeProblem(w, http.StatusBadRequest, "Missing name", "")
		return
	}
	row, err := store.LoadAddedSkillCandidate(r.Context(), s.store.DB(), name)
	if err != nil {
		s.slog.Error("LoadAddedSkillCandidate", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	if row == nil {
		writeProblem(w, http.StatusNotFound, "Added candidate not found", "")
		return
	}
	writeJSON(w, http.StatusOK, wire.AddedSkillCandidateResponse{Candidate: skillCandidateRowToWire(*row)})
}

// handleSegmentSession serves POST /v1/sessions/{id}/segment.
// Reads every event for the session, runs the segmenter, and
// writes the resulting episodes via SaveEpisodes (the existing
// /v1/episodes write path). Returns the count.
//
// Note: segmentation is pure (events → []Episode) at the store
// layer; this handler is the only place that ties the segmenter
// + SaveEpisodes write into a single endpoint so callers get
// "segment this session" semantics without two round-trips.
func (s *Server) handleSegmentSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "Missing session id", "")
		return
	}
	var req wire.SegmentSessionRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "Invalid body", err.Error())
			return
		}
	}
	evs, err := store.LoadEventsForSession(r.Context(), s.store.DB(), id, store.LoadEventsForSessionUnbounded)
	if err != nil {
		s.slog.Error("LoadEventsForSession (segment)", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	episodes := store.SegmentSession(id, evs, req.IdleGapMs)
	if _, err := store.SaveEpisodes(r.Context(), s.store.DB(), id, episodes); err != nil {
		s.slog.Error("SaveEpisodes", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	writeJSON(w, http.StatusOK, wire.SegmentSessionResponse{Episodes: len(episodes)})
}

// handleSkillCandidateUpdate serves
// POST /v1/skill-candidates/{id}/update with body
// {add_path?, body_sha256?, kind?}. Used by the merge path to
// converge the surviving candidate's stored hash + kind to the
// post-merge SKILL.md on disk.
func (s *Server) handleSkillCandidateUpdate(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid id", "")
		return
	}
	var req wire.UpdateSkillCandidateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Invalid body", err.Error())
		return
	}
	if req.AddPath != "" || req.BodySHA256 != "" {
		if req.AddPath == "" {
			writeProblem(w, http.StatusBadRequest, "add_path is required when body_sha256 is set", "")
			return
		}
		if uerr := store.UpdateSkillCandidateAddBodyHash(r.Context(), s.store.DB(), id, req.AddPath, req.BodySHA256); uerr != nil {
			if errors.Is(uerr, store.ErrSkillCandidateNotFound) {
				writeProblem(w, http.StatusNotFound, "Skill candidate not found", "")
				return
			}
			s.slog.Error("UpdateSkillCandidateAddBodyHash", "err", uerr)
			writeProblem(w, http.StatusInternalServerError, "Storage error", "")
			return
		}
	}
	if req.Kind != "" {
		if uerr := store.UpdateSkillCandidateKind(r.Context(), s.store.DB(), id, store.SkillKind(req.Kind)); uerr != nil {
			if errors.Is(uerr, store.ErrSkillCandidateNotFound) {
				writeProblem(w, http.StatusNotFound, "Skill candidate not found", "")
				return
			}
			s.slog.Error("UpdateSkillCandidateKind", "err", uerr)
			writeProblem(w, http.StatusInternalServerError, "Storage error", "")
			return
		}
	}
	writeJSON(w, http.StatusOK, wire.UpdateSkillCandidateResponse{})
}

// handleVacuum serves POST /v1/admin/vacuum. Synchronous: VACUUM
// holds an exclusive lock and the operator wants to know it
// finished before issuing follow-up work.
func (s *Server) handleVacuum(w http.ResponseWriter, r *http.Request) {
	if err := store.Vacuum(r.Context(), s.store.DB()); err != nil {
		s.slog.Error("Vacuum", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	writeJSON(w, http.StatusOK, wire.VacuumResponse{})
}

// handleDBInfo serves GET /v1/admin/db-info — page_count + page_size
// + computed bytes, used by the prune CLI to print before/after
// numbers around a vacuum.
func (s *Server) handleDBInfo(w http.ResponseWriter, r *http.Request) {
	info, err := store.QueryPageInfo(r.Context(), s.store.DB())
	if err != nil {
		s.slog.Error("QueryPageInfo", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	writeJSON(w, http.StatusOK, wire.DBPageInfoResponse{
		PageCount: info.PageCount,
		PageSize:  info.PageSize,
		Bytes:     info.Bytes(),
	})
}

// handleIngestStats serves GET /v1/admin/stats — a snapshot of
// the ingest queue's health (depth, oldest-row age, worst attempt
// count) so an operator can curl one URL instead of journal-
// grepping for worker progress. arch_review_2026_05_13 LOW #20.
func (s *Server) handleIngestStats(w http.ResponseWriter, r *http.Request) {
	stats, err := store.QueryIngestPendingStats(r.Context(), s.store.DB())
	if err != nil {
		s.slog.Error("QueryIngestPendingStats", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	resp := wire.IngestStatsResponse{
		Pending:     stats.Count,
		Capacity:    s.ingestQueueMax,
		MaxAttempts: stats.MaxAttempts,
	}
	if stats.OldestReceivedAtMs > 0 {
		resp.OldestAgeMs = time.Now().UnixMilli() - stats.OldestReceivedAtMs
		if resp.OldestAgeMs < 0 {
			// Clock skew / time-pinned test — clamp at 0 so
			// the response is never visibly nonsensical.
			resp.OldestAgeMs = 0
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
