package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// eventsPerSessionPage caps how many events the detail view
// renders. Sessions with thousands of tool calls would otherwise
// produce DOM that's pointless to scroll. v1 is read-only and
// non-paginated; if a session genuinely needs more, the user can
// drop to `aichronicles search --session=<id>`.
const eventsPerSessionPage = 200

// errSessionNotFound is the sentinel loadSessionDetail returns
// when the requested id has no matching sessions row. The
// handler maps this to HTTP 404 specifically; any other error is
// a 500.
var errSessionNotFound = errors.New("session not found")

// sessionDetailHandler renders /sessions/{id}: header fields, a
// cached LLM summary if one exists, and the event timeline.
// Accepts the full session UUID; the sessions list page already
// links with the full id, so prefix resolution stays a CLI/MCP
// concern for now.
func (s *Server) sessionDetailHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := loadSessionDetail(r.Context(), s.store, id)
	if err != nil {
		if errors.Is(err, errSessionNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("sessionDetailHandler: load", "id", id, "err", err)
		http.Error(w, "could not load session", http.StatusInternalServerError)
		return
	}
	s.render(w, r, "session", detail)
}

// loadSessionDetail builds the SessionDetail view for one id.
// Three queries: the session row, the latest cached summary
// (filtered by kind=summary), and the most recent N events.
func loadSessionDetail(ctx context.Context, st *store.Store, id string) (*SessionDetail, error) {
	header, err := loadSessionHeader(ctx, st, id)
	if err != nil {
		return nil, err
	}
	summary, err := loadLatestSummary(ctx, st, id)
	if err != nil {
		return nil, fmt.Errorf("load summary: %w", err)
	}
	events, err := loadEventRows(ctx, st, id, eventsPerSessionPage)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}
	header.Summary = summary
	header.Events = events
	return header, nil
}

// loadSessionHeader pulls the sessions row for one id and
// renders its display fields. Returns errSessionNotFound when
// the row doesn't exist; the handler maps that to 404.
func loadSessionHeader(ctx context.Context, st *store.Store, id string) (*SessionDetail, error) {
	const q = `
		SELECT id, started_at_ms, ended_at_ms, event_count, cwd,
		       source_agent, source_session_id
		  FROM sessions WHERE id = ?`
	row := st.DB().QueryRowContext(ctx, q, id)

	var (
		gotID           string
		startedMs       sql.NullInt64
		endedMs         sql.NullInt64
		eventCount      int
		cwd             sql.NullString
		sourceAgent     string
		sourceSessionID string
	)
	if err := row.Scan(&gotID, &startedMs, &endedMs, &eventCount, &cwd,
		&sourceAgent, &sourceSessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errSessionNotFound
		}
		return nil, fmt.Errorf("scan session row: %w", err)
	}

	return &SessionDetail{
		Title:           "session " + shortID(gotID),
		ID:              gotID,
		ShortID:         shortID(gotID),
		Cwd:             orDash(cwd),
		StartedAt:       absoluteOrDash(startedMs),
		EndedAt:         endedOrActive(endedMs),
		EventCount:      eventCount,
		SourceAgent:     sourceAgent,
		SourceSessionID: sourceSessionID,
		ResumeCommand:   buildResumeCommand(sourceAgent, sourceSessionID, cwd),
	}, nil
}

// buildResumeCommand renders the shell one-liner the Resume
// button copies to the clipboard. cd-then-launch so the agent
// resumes against the same workspace it was captured in;
// `claude --resume` keys off cwd, not just the session id.
//
// Returns "" for unknown / empty agents — the template branches
// on that to hide the button rather than show a copy action that
// pastes "" into the user's terminal.
func buildResumeCommand(agent, sourceSessionID string, cwd sql.NullString) string {
	if sourceSessionID == "" {
		return ""
	}
	switch agent {
	case "claude-code":
		base := "claude --resume " + sourceSessionID
		if cwd.Valid && cwd.String != "" {
			return "cd " + cwd.String + " && " + base
		}
		return base
	default:
		// codex / other agents have their own resume invocations
		// we haven't modelled yet; emit nothing rather than guess.
		return ""
	}
}

// loadLatestSummary returns the most recent summary llm_outputs
// row for sessionID, parsed into the rendering shape. nil when
// no summary has been generated for this session yet.
func loadLatestSummary(ctx context.Context, st *store.Store, sessionID string) (*SessionSummary, error) {
	outs, err := store.LoadLLMOutputsForSession(ctx, st.DB(), sessionID)
	if err != nil {
		return nil, err
	}
	// Pick the most recent kind=summary output. LoadLLMOutputsForSession
	// orders by created_at_ms DESC, so the first match is the latest.
	for _, o := range outs {
		if o.Kind != store.LLMKindSummary {
			continue
		}
		var parsed prompts.SummaryResult
		if err := json.Unmarshal([]byte(o.Body), &parsed); err != nil {
			// Bad cached body shouldn't break the page —
			// surface the raw body in WhatWasDone so the user
			// can still see what's there.
			return &SessionSummary{
				Topic:       "(unparseable cached summary)",
				WhatWasDone: []string{o.Body},
				Model:       o.Model,
				GeneratedAt: time.UnixMilli(o.CreatedAtMs).UTC().Format("2006-01-02 15:04 UTC"),
			}, nil
		}
		out := &SessionSummary{
			Topic:       parsed.Topic,
			WhatWasDone: parsed.WhatWasDone,
			Unresolved:  parsed.Unresolved,
			KeyFiles:    parsed.KeyFiles,
			Model:       o.Model,
			GeneratedAt: time.UnixMilli(o.CreatedAtMs).UTC().Format("2006-01-02 15:04 UTC"),
		}
		for _, l := range parsed.Links {
			out.Links = append(out.Links, SummaryLink{URL: l.URL, UsedFor: l.UsedFor})
		}
		return out, nil
	}
	return nil, nil
}

// loadEventRows pulls the most recent `limit` events for the
// session and renders each one for the timeline.
func loadEventRows(ctx context.Context, st *store.Store, sessionID string, limit int) ([]EventRow, error) {
	events, err := store.LoadEventsForSession(ctx, st.DB(), sessionID, limit)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]EventRow, 0, len(events))
	for _, e := range events {
		out = append(out, EventRow{
			When:    relativeTime(e.TsSourceMs, now),
			Kind:    e.Kind,
			Role:    nullStr(e.Role),
			Tool:    nullStr(e.ToolName),
			Snippet: truncatePreview(e.ContentText),
		})
	}
	return out, nil
}

// absoluteOrDash renders a started/ended timestamp in a
// machine-readable UTC form, or "-" when the column is NULL.
func absoluteOrDash(ms sql.NullInt64) string {
	if !ms.Valid || ms.Int64 == 0 {
		return "-"
	}
	return time.UnixMilli(ms.Int64).UTC().Format("2006-01-02 15:04 UTC")
}

// endedOrActive renders the session-end timestamp. NULL means
// "no SessionEnd captured yet" — show "(active)" so the user
// can tell at a glance.
func endedOrActive(ms sql.NullInt64) string {
	if !ms.Valid || ms.Int64 == 0 {
		return "(active)"
	}
	return time.UnixMilli(ms.Int64).UTC().Format("2006-01-02 15:04 UTC")
}

// nullStr returns the underlying string of a sql.NullString or
// "" when invalid. Mirrors orDash without the dash placeholder
// for callers that prefer empty over "-".
func nullStr(s sql.NullString) string {
	if !s.Valid {
		return ""
	}
	return s.String
}
