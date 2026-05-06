package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/timefmt"
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
//
// Accepts either the full session UUID or any unique prefix —
// matching the resolution rule used by `aichronicles sessions`
// and the MCP get_summary tool, so a short id pasted from
// either surface lands on the right page. When a prefix
// resolves to one full id, redirect to the canonical URL so
// links/bookmarks normalise.
func (s *Server) sessionDetailHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	resolved, err := store.ResolveSessionIDPrefix(r.Context(), s.store.DB(), id)
	switch {
	case errors.Is(err, store.ErrNoSuchSession):
		http.NotFound(w, r)
		return
	case errors.Is(err, store.ErrAmbiguousSessionPrefix):
		// Surface the candidate list the store error embeds so
		// the user can pick. 400 (not 404): the resource is
		// reachable, the request was just under-specified.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		s.log.Error("sessionDetailHandler: resolve prefix", "id", id, "err", err)
		http.Error(w, "could not resolve session", http.StatusInternalServerError)
		return
	}
	if resolved != id {
		http.Redirect(w, r, "/sessions/"+resolved, http.StatusFound)
		return
	}

	detail, err := loadSessionDetail(r.Context(), s.store, resolved)
	if err != nil {
		if errors.Is(err, errSessionNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("sessionDetailHandler: load", "id", resolved, "err", err)
		http.Error(w, "could not load session", http.StatusInternalServerError)
		return
	}
	s.render(w, r, "session", detail)
}

// loadSessionDetail builds the SessionDetail view for one id.
// Four queries: the session row, the latest cached summary
// (filtered by kind=summary), the most recent N events, and the
// session_links pointing in either direction.
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
	related, err := loadRelatedSessions(ctx, st, id)
	if err != nil {
		return nil, fmt.Errorf("load related sessions: %w", err)
	}
	episodes, err := loadEpisodeRows(ctx, st, id)
	if err != nil {
		return nil, fmt.Errorf("load episodes: %w", err)
	}
	header.Summary = summary
	header.Events = events
	header.RelatedSessions = related
	header.Episodes = episodes
	return header, nil
}

// loadEpisodeRows fetches the segmenter's output for one session
// and renders each row for the Episodes section on the detail page.
// Returns an empty slice (not error) when the daemon hasn't
// segmented this session yet — the template hides the section.
func loadEpisodeRows(ctx context.Context, st *store.Store, sessionID string) ([]EpisodeRow, error) {
	episodes, err := store.LoadEpisodesBySession(ctx, st.DB(), sessionID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]EpisodeRow, 0, len(episodes))
	for _, ep := range episodes {
		row := EpisodeRow{
			Ordinal:       ep.Ordinal,
			Started:       relativeTime(ep.StartedAtMs, now),
			Ended:         relativeTime(ep.EndedAtMs, now),
			IntentSummary: ep.IntentSummary,
			EventCount:    ep.EventCount,
		}
		if ep.Cwd.Valid {
			row.Cwd = ep.Cwd.String
		}
		out = append(out, row)
	}
	return out, nil
}

// loadRelatedSessions assembles the "Related sessions" sidebar.
// Pulls outgoing + incoming links and groups them by kind. For
// each linked session id we fetch a topic from the latest summary
// (best-effort — empty when the linked session hasn't been
// summarized).
//
// Returns nil when neither direction has any links, so the
// template can hide the entire sidebar with a single nil-check.
func loadRelatedSessions(ctx context.Context, st *store.Store, id string) ([]RelatedSessionGroup, error) {
	outgoing, err := store.LoadSessionLinksFrom(ctx, st.DB(), id)
	if err != nil {
		return nil, err
	}
	incoming, err := store.LoadSessionLinksTo(ctx, st.DB(), id)
	if err != nil {
		return nil, err
	}
	if len(outgoing) == 0 && len(incoming) == 0 {
		return nil, nil
	}

	// Topics for every distinct linked id, batch-fetched so the
	// sidebar doesn't issue one query per row.
	relatedIDs := make(map[string]struct{})
	for _, l := range outgoing {
		relatedIDs[l.ToSessionID] = struct{}{}
	}
	for _, l := range incoming {
		relatedIDs[l.FromSessionID] = struct{}{}
	}
	topics, err := loadTopicsForSessions(ctx, st, relatedIDs)
	if err != nil {
		return nil, err
	}

	// Group by kind in canonical order. Outgoing entries first
	// inside each group ("this session builds on X"), incoming
	// after ("Y builds on this session") so a reader's eye lands
	// on the more proximate framing first.
	type bucket struct {
		out []store.SessionLink
		in  []store.SessionLink
	}
	by := make(map[string]*bucket)
	for _, l := range outgoing {
		b := by[l.Kind]
		if b == nil {
			b = &bucket{}
			by[l.Kind] = b
		}
		b.out = append(b.out, l)
	}
	for _, l := range incoming {
		b := by[l.Kind]
		if b == nil {
			b = &bucket{}
			by[l.Kind] = b
		}
		b.in = append(b.in, l)
	}

	groups := make([]RelatedSessionGroup, 0, len(by))
	for _, k := range store.SessionLinkKinds {
		b, ok := by[k]
		if !ok {
			continue
		}
		entries := make([]RelatedSessionEntry, 0, len(b.out)+len(b.in))
		for _, l := range b.out {
			entries = append(entries, RelatedSessionEntry{
				Direction: "out",
				ID:        l.ToSessionID,
				ShortID:   shortID(l.ToSessionID),
				Topic:     topics[l.ToSessionID],
				Rationale: l.Rationale,
			})
		}
		for _, l := range b.in {
			entries = append(entries, RelatedSessionEntry{
				Direction: "in",
				ID:        l.FromSessionID,
				ShortID:   shortID(l.FromSessionID),
				Topic:     topics[l.FromSessionID],
				Rationale: l.Rationale,
			})
		}
		groups = append(groups, RelatedSessionGroup{
			Kind:    k,
			Label:   relatedSessionLabel(k),
			Entries: entries,
		})
	}
	return groups, nil
}

// relatedSessionLabel maps a SessionLinkKinds value to the human
// label rendered in the sidebar header. Kept symmetrical
// regardless of direction — the template adds "(this session)" /
// "(other session)" framing per-entry rather than per-group.
func relatedSessionLabel(kind string) string {
	switch kind {
	case store.SessionLinkBuildsOn:
		return "Builds on"
	case store.SessionLinkRepeatsFailureOf:
		return "Repeats failure of"
	case store.SessionLinkSupersedes:
		return "Supersedes"
	case store.SessionLinkRelated:
		return "Related"
	default:
		return kind
	}
}

// loadTopicsForSessions fetches the latest summary topic for each
// id in the set. Missing ids are returned with empty string —
// callers fall back to the short id when topic is "".
func loadTopicsForSessions(ctx context.Context, st *store.Store, ids map[string]struct{}) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// Build a placeholder list for IN (?, ?, ?…).
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	q := `
	  SELECT s.id, COALESCE(s.summary_topic, '')
	    FROM sessions s
	   WHERE s.id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, topic string
		if err := rows.Scan(&id, &topic); err != nil {
			return nil, err
		}
		out[id] = topic
	}
	return out, rows.Err()
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

	// The resume command MUST cd into the session's *start* cwd, not
	// sessions.cwd: Claude looks up transcripts under
	// ~/.claude/projects/<encoded-cwd>/ keyed by the cwd at session
	// start. The trigger keeps sessions.cwd as the latest cwd seen
	// (useful for "where was this session most recently working"),
	// which breaks resume whenever the user cd'd mid-session. Falls
	// back to sessions.cwd if the lookup turns up nothing.
	resumeCwd, err := store.LoadSessionStartCwd(ctx, st.DB(), gotID)
	if err != nil {
		return nil, fmt.Errorf("load start cwd: %w", err)
	}
	if !resumeCwd.Valid {
		resumeCwd = cwd
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
		ResumeCommand:   buildResumeCommand(sourceAgent, sourceSessionID, resumeCwd),
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
	var base string
	switch agent {
	case "claude-code":
		base = "claude --resume " + sourceSessionID
	case "gemini-cli":
		// gemini-cli's `--help` advertises only `--resume <index>`
		// or `--resume latest`, but the binary also accepts the
		// session UUID directly (verified end-to-end:
		// `gemini --resume <uuid>` correctly carries the prior
		// session history). We emit the UUID form so the resume
		// button doesn't depend on the volatile index ordering of
		// `--list-sessions` (which changes every time a new
		// session is created).
		base = "gemini --resume " + sourceSessionID
	default:
		// codex / other agents have their own resume invocations
		// we haven't modelled yet; emit nothing rather than guess.
		return ""
	}
	if cwd.Valid && cwd.String != "" {
		return "cd " + cwd.String + " && " + base
	}
	return base
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
			Role:    e.Role.OrEmpty(),
			Tool:    e.ToolName.OrEmpty(),
			Snippet: truncatePreviewString(e.ContentText.OrEmpty()),
		})
	}
	return out, nil
}

// absoluteOrDash renders a started/ended timestamp in a
// machine-readable UTC form, or "-" when the column is NULL.
// Wraps internal/timefmt so the date layout stays consistent
// with the CLI table output.
func absoluteOrDash(ms sql.NullInt64) string {
	return timefmt.AbsoluteOrDash(ms)
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
