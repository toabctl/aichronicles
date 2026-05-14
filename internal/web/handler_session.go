package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/wire"
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
	resolved, err := s.api.ResolveSession(r.Context(), id)
	switch {
	case errors.Is(err, apiclient.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, apiclient.ErrConflict):
		// Ambiguous prefix: the api surfaces the candidate list in
		// the problem detail. 400 (not 404): the resource is
		// reachable, the request was just under-specified.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		s.internalError(w, "sessionDetailHandler: resolve prefix id="+id, "could not resolve session", err)
		return
	}
	if resolved != id {
		http.Redirect(w, r, "/sessions/"+resolved, http.StatusFound)
		return
	}

	detail, err := loadSessionDetail(r.Context(), s, resolved)
	if err != nil {
		if errors.Is(err, errSessionNotFound) {
			http.NotFound(w, r)
			return
		}
		s.internalError(w, "sessionDetailHandler: load id="+resolved, "could not load session", err)
		return
	}
	s.render(w, r, "session", detail)
}

// loadSessionDetail builds the SessionDetail view for one id by
// fanning a few apiclient calls — the daemon owns SQL access.
func loadSessionDetail(ctx context.Context, s *Server, id string) (*SessionDetail, error) {
	header, err := loadSessionHeader(ctx, s, id)
	if err != nil {
		return nil, err
	}
	summary, err := loadLatestSummary(ctx, s, id)
	if err != nil {
		return nil, fmt.Errorf("load summary: %w", err)
	}
	events, err := loadEventRows(ctx, s, id, eventsPerSessionPage)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}
	related, err := loadRelatedSessions(ctx, s, id)
	if err != nil {
		return nil, fmt.Errorf("load related sessions: %w", err)
	}
	episodes, err := loadEpisodeRows(ctx, s, id)
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
func loadEpisodeRows(ctx context.Context, s *Server, sessionID string) ([]EpisodeRow, error) {
	resp, err := s.api.Episodes(ctx, wire.EpisodeListRequest{SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]EpisodeRow, 0, len(resp.Episodes))
	for _, ep := range resp.Episodes {
		row := EpisodeRow{
			Ordinal:       ep.Ordinal,
			Started:       relativeTime(ep.StartedAtMs, now),
			Ended:         relativeTime(ep.EndedAtMs, now),
			IntentSummary: ep.IntentSummary,
			EventCount:    ep.EventCount,
		}
		if ep.Cwd != nil {
			row.Cwd = *ep.Cwd
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
func loadRelatedSessions(ctx context.Context, s *Server, id string) ([]RelatedSessionGroup, error) {
	outgoing, err := s.api.SessionLinksFrom(ctx, id)
	if err != nil {
		return nil, err
	}
	incoming, err := s.api.SessionLinksTo(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(outgoing) == 0 && len(incoming) == 0 {
		return nil, nil
	}

	// Topics for every distinct linked id, batch-fetched so the
	// sidebar doesn't issue one query per row.
	idSet := make(map[string]struct{})
	for _, l := range outgoing {
		idSet[l.ToSessionID] = struct{}{}
	}
	for _, l := range incoming {
		idSet[l.FromSessionID] = struct{}{}
	}
	topics, err := loadTopicsForSessions(ctx, s, idSet)
	if err != nil {
		return nil, err
	}

	// Group by kind in canonical order. Outgoing entries first
	// inside each group ("this session builds on X"), incoming
	// after ("Y builds on this session") so a reader's eye lands
	// on the more proximate framing first.
	type bucket struct {
		out []wire.SessionLinkRow
		in  []wire.SessionLinkRow
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
	for _, k := range wire.SessionLinkKinds {
		b, ok := by[k]
		if !ok {
			continue
		}
		entries := make([]RelatedSessionEntry, 0, len(b.out)+len(b.in))
		for _, l := range b.out {
			entries = append(entries, RelatedSessionEntry{
				Direction: "out",
				ID:        l.ToSessionID,
				ShortID:   preview.ShortID(l.ToSessionID),
				Topic:     topics[l.ToSessionID],
				Rationale: l.Rationale,
			})
		}
		for _, l := range b.in {
			entries = append(entries, RelatedSessionEntry{
				Direction: "in",
				ID:        l.FromSessionID,
				ShortID:   preview.ShortID(l.FromSessionID),
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
	case wire.SessionLinkBuildsOn:
		return "Builds on"
	case wire.SessionLinkRepeatsFailureOf:
		return "Repeats failure of"
	case wire.SessionLinkSupersedes:
		return "Supersedes"
	case wire.SessionLinkRelated:
		return "Related"
	default:
		return kind
	}
}

// loadTopicsForSessions fetches the latest summary topic for each
// id in the set. Missing ids are returned with empty string —
// callers fall back to the short id when topic is "". Backed by
// the /v1/summaries/batch endpoint; sessions without a cached
// summary surface as empty topics.
func loadTopicsForSessions(ctx context.Context, s *Server, ids map[string]struct{}) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	summaries, err := s.api.SummariesBatch(ctx, idList)
	if err != nil {
		return nil, err
	}
	for id, sm := range summaries {
		out[id] = parseSummaryTopic(sm.Body)
	}
	return out, nil
}

// loadSessionHeader pulls the session detail row from the api and
// renders its display fields. Returns errSessionNotFound when the
// row doesn't exist; the handler maps that to 404.
//
// The detail endpoint (/v1/sessions/{id}) returns the SessionDigest
// shape, which lacks source_agent / source_session_id — those are
// fed into the Resume button via a second tiny call once the api
// exposes them. For now we leave the Resume command empty when we
// can't compute it; the template hides the button.
func loadSessionHeader(ctx context.Context, s *Server, id string) (*SessionDetail, error) {
	digest, err := s.api.Session(ctx, id)
	if err != nil {
		if errors.Is(err, apiclient.ErrNotFound) {
			return nil, errSessionNotFound
		}
		return nil, fmt.Errorf("get session: %w", err)
	}

	// The resume command MUST cd into the session's *start* cwd, not
	// sessions.cwd: Claude looks up transcripts under
	// ~/.claude/projects/<encoded-cwd>/ keyed by the cwd at session
	// start. Falls back to sessions.cwd if no start_cwd was recorded.
	resumeCwd, err := s.api.SessionStartCwd(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load start cwd: %w", err)
	}
	if resumeCwd == nil {
		resumeCwd = digest.Cwd
	}

	return &SessionDetail{
		Title:           "session " + preview.ShortID(digest.ID),
		ID:              digest.ID,
		ShortID:         preview.ShortID(digest.ID),
		Cwd:             orDashPtr(digest.Cwd),
		StartedAt:       absoluteOrDashPtr(digest.StartedAtMs),
		EndedAt:         endedOrActivePtr(digest.EndedAtMs),
		EventCount:      digest.EventCount,
		SourceAgent:     digest.SourceAgent,
		SourceSessionID: digest.SourceSessionID,
		ResumeCommand:   buildResumeCommandPtr(digest.SourceAgent, digest.SourceSessionID, resumeCwd),
	}, nil
}

// buildResumeCommandPtr renders the shell one-liner the Resume
// button copies to the clipboard. cd-then-launch so the agent
// resumes against the same workspace it was captured in;
// `claude --resume` keys off cwd, not just the session id.
//
// Returns "" for unknown / empty agents — the template branches
// on that to hide the button rather than show a copy action that
// pastes "" into the user's terminal.
func buildResumeCommandPtr(agent, sourceSessionID string, cwd *string) string {
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
	if cwd != nil && *cwd != "" {
		return "cd " + *cwd + " && " + base
	}
	return base
}

// loadLatestSummary returns the most recent summary llm_outputs
// row for sessionID, parsed into the rendering shape. nil when
// no summary has been generated for this session yet.
func loadLatestSummary(ctx context.Context, s *Server, sessionID string) (*SessionSummary, error) {
	outs, err := s.api.SessionLLMOutputs(ctx, sessionID, "", 0)
	if err != nil {
		return nil, err
	}
	// Pick the most recent kind=summary output. SessionLLMOutputs
	// orders by created_at_ms DESC, so the first match is the latest.
	for _, o := range outs {
		if o.Kind != string(wire.LLMKindSummary) {
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
func loadEventRows(ctx context.Context, s *Server, sessionID string, limit int) ([]EventRow, error) {
	resp, err := s.api.SessionEvents(ctx, sessionID, limit, false)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]EventRow, 0, len(resp.Events))
	for _, e := range resp.Events {
		out = append(out, EventRow{
			When:    relativeTime(e.TsSourceMs, now),
			Kind:    e.Kind,
			Role:    derefStringOrEmpty(e.Role),
			Tool:    derefStringOrEmpty(e.ToolName),
			Snippet: truncatePreviewString(derefStringOrEmpty(e.ContentText)),
		})
	}
	return out, nil
}

// absoluteOrDashPtr renders a started/ended timestamp in a
// machine-readable UTC form, or "-" when the pointer is nil.
// Pointer-aware sibling of internal/timefmt.AbsoluteOrDash, used
// where wire types deliver *int64 instead of sql.NullInt64.
func absoluteOrDashPtr(ms *int64) string {
	if ms == nil || *ms == 0 {
		return "-"
	}
	return time.UnixMilli(*ms).UTC().Format("2006-01-02 15:04 UTC")
}

// endedOrActivePtr renders the session-end timestamp. nil means
// "no SessionEnd captured yet" — show "(active)" so the user can
// tell at a glance.
func endedOrActivePtr(ms *int64) string {
	if ms == nil || *ms == 0 {
		return "(active)"
	}
	return time.UnixMilli(*ms).UTC().Format("2006-01-02 15:04 UTC")
}

// derefStringOrEmpty unwraps a *string, returning "" for nil. Tiny
// local helper used by per-event projection where wire types ship
// *string for nullable columns.
func derefStringOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
