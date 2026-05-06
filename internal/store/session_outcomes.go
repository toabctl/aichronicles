package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
)

// OutcomeLabel is the coarse derived verdict for a session. The
// `_likely` suffix is deliberate: these are heuristics over
// observational data, not ground truth. Downstream consumers read
// them as priors, not facts.
type OutcomeLabel string

const (
	// OutcomeSuccessLikely — clean activity, no failure markers.
	// Defined as: tool_use_count >= 1 AND tool_failure_count == 0
	// AND git_undo_count == 0 AND prompt_repeat_count == 0 AND
	// error_count == 0.
	OutcomeSuccessLikely OutcomeLabel = "success_likely"

	// OutcomeFailureLikely — strong failure signals. Defined as
	// any one of: tool_failure_count >= toolFailureFloor (where the
	// floor scales with session size — see toolFailureFloor);
	// git_undo_count >= 1; prompt_repeat_count >= 2; or "session
	// ended on tool_failure or error" (last_event_kind in
	// failure-shaped values AND tool_failure_count + error_count
	// >= 1).
	OutcomeFailureLikely OutcomeLabel = "failure_likely"

	// OutcomeMixed — real activity with weak failure signals. The
	// session got somewhere but had friction. Used when the row
	// fails the success bar but doesn't trip the failure bar.
	OutcomeMixed OutcomeLabel = "mixed"

	// OutcomeUnknown — too thin to label. tool_use_count == 0 and
	// user_prompt_count <= 1 — typically aborted preambles, never
	// got far enough to leave a trail.
	OutcomeUnknown OutcomeLabel = "unknown"
)

// SessionOutcome is one row of session_outcomes. Field meanings:
// see migration 017 and the ComputeSessionOutcome rule comment.
type SessionOutcome struct {
	SessionID         string
	ComputedAtMs      int64
	UserPromptCount   int
	ToolUseCount      int
	ToolFailureCount  int
	ErrorCount        int
	CompactCount      int
	GitUndoCount      int
	PromptRepeatCount int
	LastEventKind     sql.NullString
	Outcome           OutcomeLabel
}

// gitUndoRE matches a single shell command (no chain operators)
// that unambiguously undoes work in a git checkout. Conservative on
// purpose: plain `git reset HEAD` (which only unstages) is excluded
// because it doesn't lose work, and bare `git checkout <branch>` is
// excluded because switching branches is not undoing — only `--`
// (path scope) or `.` (full WT) counts. CLAUDE.md rule 7: rather
// miss a subtle undo than count a neutral command.
//
// Callers split chained commands on `&&`/`||`/`;` first and match
// each link independently — so `cd repo && git reset --hard` still
// trips the detector via the second link. See splitShellChain.
var gitUndoRE = regexp.MustCompile(`^\s*git\s+(?:reset\s+--hard\b|revert\b|checkout\s+(?:--|\.)|restore\b|stash\s+(?:push|save)\b|stash\s*$)`)

// shellChainSep splits a shell command on the canonical chaining
// operators &&, ||, ;.
var shellChainSep = regexp.MustCompile(`\s*(?:&&|\|\||;)\s*`)

// promptWhitespaceRE collapses any whitespace run to a single space
// for prompt-repeat normalisation.
var promptWhitespaceRE = regexp.MustCompile(`\s+`)

// ComputeSessionOutcome reads the events + extractions for sessionID
// and produces the SessionOutcome that captures its outcome signals.
// Pure read; the caller persists via SaveSessionOutcome.
//
// Returns ErrSessionNotFound when sessionID has no row in `sessions`
// (vs. a session that exists but has zero events — that one returns a
// SessionOutcome with all-zero counts and Outcome=unknown).
func ComputeSessionOutcome(ctx context.Context, db *sql.DB, sessionID string) (SessionOutcome, error) {
	if sessionID == "" {
		return SessionOutcome{}, errors.New("ComputeSessionOutcome: session_id is required")
	}

	// Sanity-check the session exists. A FK insert on a non-existent
	// session_id would fail with a confusing FK error; surface a
	// clear error here instead.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM sessions WHERE id = ? LIMIT 1`, sessionID,
	).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionOutcome{}, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
		}
		return SessionOutcome{}, fmt.Errorf("check session: %w", err)
	}

	out := SessionOutcome{
		SessionID:    sessionID,
		ComputedAtMs: time.Now().UnixMilli(),
	}

	// Aggregate event counts in one query. Each kind is a
	// CASE-WHEN over one indexed scan of events for this session.
	const aggQuery = `
SELECT
    SUM(CASE WHEN kind = ? THEN 1 ELSE 0 END) AS user_prompts,
    SUM(CASE WHEN kind = ? THEN 1 ELSE 0 END) AS tool_uses,
    SUM(CASE WHEN kind = ? THEN 1 ELSE 0 END) AS tool_failures,
    SUM(CASE WHEN kind = ? THEN 1 ELSE 0 END) AS errors,
    SUM(CASE WHEN kind = ? THEN 1 ELSE 0 END) AS compacts
FROM events WHERE session_id = ?`
	var ups, tus, tfs, errs, cmps sql.NullInt64
	if err := db.QueryRowContext(ctx, aggQuery,
		events.KindUserPrompt,
		events.KindToolUse,
		events.KindToolFailure,
		events.KindError,
		events.KindCompactStart,
		sessionID,
	).Scan(&ups, &tus, &tfs, &errs, &cmps); err != nil {
		return SessionOutcome{}, fmt.Errorf("aggregate event kinds: %w", err)
	}
	out.UserPromptCount = int(ups.Int64)
	out.ToolUseCount = int(tus.Int64)
	out.ToolFailureCount = int(tfs.Int64)
	out.ErrorCount = int(errs.Int64)
	out.CompactCount = int(cmps.Int64)

	// Last event kind: the chronologically last event for this
	// session. Tie-break on rowid like the start_cwd trigger does.
	if err := db.QueryRowContext(ctx,
		`SELECT kind FROM events WHERE session_id = ?
		 ORDER BY ts_source_ms DESC, rowid DESC LIMIT 1`,
		sessionID,
	).Scan(&out.LastEventKind); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SessionOutcome{}, fmt.Errorf("last event kind: %w", err)
	}

	// Git-undo count: scan shell_command extractions for this
	// session; match each first-of-chain command against gitUndoRE.
	shells, err := loadShellCommands(ctx, db, sessionID)
	if err != nil {
		return SessionOutcome{}, fmt.Errorf("load shell commands: %w", err)
	}
	out.GitUndoCount = countGitUndo(shells)

	// Prompt-repeat count: walk user_prompts in chronological order
	// and count consecutive duplicates after normalisation.
	prompts, err := loadUserPromptTexts(ctx, db, sessionID)
	if err != nil {
		return SessionOutcome{}, fmt.Errorf("load user prompts: %w", err)
	}
	out.PromptRepeatCount = countConsecutiveRepeats(prompts)

	out.Outcome = deriveOutcomeLabel(out)
	return out, nil
}

// ErrSessionNotFound is returned by ComputeSessionOutcome (and
// LoadSessionOutcome on a strict variant if added later) when the
// sessionID has no row in the sessions table. Distinct from "row
// computed and outcome=unknown."
var ErrSessionNotFound = errors.New("session not found")

// SaveSessionOutcome upserts the row into session_outcomes. The PK
// is session_id, so re-saving overwrites — the caller calling
// ComputeSessionOutcome again is the recompute path.
func SaveSessionOutcome(ctx context.Context, db *sql.DB, o SessionOutcome) error {
	if o.SessionID == "" {
		return errors.New("SaveSessionOutcome: session_id is required")
	}
	if o.Outcome == "" {
		return errors.New("SaveSessionOutcome: outcome label is required")
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO session_outcomes(
			session_id, computed_at_ms,
			user_prompt_count, tool_use_count, tool_failure_count,
			error_count, compact_count,
			git_undo_count, prompt_repeat_count,
			last_event_kind, outcome
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			computed_at_ms      = excluded.computed_at_ms,
			user_prompt_count   = excluded.user_prompt_count,
			tool_use_count      = excluded.tool_use_count,
			tool_failure_count  = excluded.tool_failure_count,
			error_count         = excluded.error_count,
			compact_count       = excluded.compact_count,
			git_undo_count      = excluded.git_undo_count,
			prompt_repeat_count = excluded.prompt_repeat_count,
			last_event_kind     = excluded.last_event_kind,
			outcome             = excluded.outcome`,
		o.SessionID, o.ComputedAtMs,
		o.UserPromptCount, o.ToolUseCount, o.ToolFailureCount,
		o.ErrorCount, o.CompactCount,
		o.GitUndoCount, o.PromptRepeatCount,
		o.LastEventKind, string(o.Outcome),
	)
	if err != nil {
		return fmt.Errorf("save session_outcomes: %w", err)
	}
	return nil
}

// EnsureSessionOutcome returns the cached outcome row for sessionID,
// computing-and-persisting a fresh one if none exists yet. Wraps
// LoadSessionOutcome + ComputeSessionOutcome + SaveSessionOutcome
// for the common digest-enrichment call pattern.
//
// Stale-cache behaviour: if a row exists but is older than the
// session's latest event, it is returned as-is — outcome
// computation is meant to run after the session settles (the
// induction-sweeper idle threshold is the canonical "settled"
// gate), so a stale row is unusual and refreshing it would mask
// the staleness from callers who care. A future
// RecomputeSessionOutcome can fix that explicitly.
//
// Outcome enrichment is best-effort: callers typically log the
// error and proceed without the cue rather than failing the
// downstream feature.
func EnsureSessionOutcome(ctx context.Context, db *sql.DB, sessionID string) (*SessionOutcome, error) {
	existing, err := LoadSessionOutcome(ctx, db, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load outcome: %w", err)
	}
	if existing != nil {
		return existing, nil
	}
	computed, err := ComputeSessionOutcome(ctx, db, sessionID)
	if err != nil {
		return nil, fmt.Errorf("compute outcome: %w", err)
	}
	if err := SaveSessionOutcome(ctx, db, computed); err != nil {
		return nil, fmt.Errorf("save outcome: %w", err)
	}
	return &computed, nil
}

// LoadSessionOutcome returns the outcome row for sessionID, or nil if
// none has been computed yet. Distinct from ComputeSessionOutcome:
// this is a pure read, no derivation.
func LoadSessionOutcome(ctx context.Context, db *sql.DB, sessionID string) (*SessionOutcome, error) {
	row := db.QueryRowContext(ctx,
		`SELECT session_id, computed_at_ms,
		        user_prompt_count, tool_use_count, tool_failure_count,
		        error_count, compact_count,
		        git_undo_count, prompt_repeat_count,
		        last_event_kind, outcome
		   FROM session_outcomes WHERE session_id = ?`,
		sessionID)
	var o SessionOutcome
	var label string
	switch err := row.Scan(
		&o.SessionID, &o.ComputedAtMs,
		&o.UserPromptCount, &o.ToolUseCount, &o.ToolFailureCount,
		&o.ErrorCount, &o.CompactCount,
		&o.GitUndoCount, &o.PromptRepeatCount,
		&o.LastEventKind, &label,
	); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("scan session_outcomes: %w", err)
	}
	o.Outcome = OutcomeLabel(label)
	return &o, nil
}

// FailureShape is one row of the contrastive-corpus query that
// surfaces sessions where things went wrong. Used by propose to
// give the LLM a NEGATIVE-example stanza alongside the recurring-
// pattern positive corpus: "what failure shapes recur?" — same
// data shape that ExpeL's contrastive insight extraction needs.
//
// Title is the most informative one-line label for the session
// (summary_topic when available, else the first user_prompt
// truncated). FailureMarkers describes WHY this session is in
// the negative corpus: which counters non-zero, plus a one-word
// label for the dominant failure mode (so the LLM can group them).
type FailureShape struct {
	SessionID         string
	EndedAtMs         sql.NullInt64
	Cwd               sql.NullString
	Title             string
	ToolFailureCount  int
	GitUndoCount      int
	PromptRepeatCount int
	LastEventKind     sql.NullString
}

// LoadFailureShapes returns sessions whose computed outcome is
// failure_likely (or mixed with concrete failure markers) within
// the given time window. Newest-first, capped at limit.
//
// Used by propose: the LLM sees these alongside the success-
// shaped digest corpus and is instructed (system rule 13) to
// consider skills that PREVENT or short-circuit the recurring
// failure modes — not just consolidate observed positive
// patterns. This is the contrastive half of ExpeL.
//
// limit ≤0 falls back to 8 — enough to expose a recurring
// failure mode without bloating the propose prompt; failure
// shapes are dense (each row carries concrete counters) so a
// smaller cap suffices than for the positive digest.
func LoadFailureShapes(ctx context.Context, db *sql.DB, sinceMs int64, limit int) ([]FailureShape, error) {
	if limit <= 0 {
		limit = 8
	}
	const q = `
SELECT s.id,
       s.ended_at_ms,
       s.cwd,
       COALESCE(NULLIF(s.summary_topic, ''),
                NULLIF(s.first_prompt_text, ''),
                '') AS title,
       so.tool_failure_count,
       so.git_undo_count,
       so.prompt_repeat_count,
       so.last_event_kind
  FROM sessions s
  JOIN session_outcomes so ON so.session_id = s.id
 WHERE so.outcome = 'failure_likely'
   AND COALESCE(s.ended_at_ms, s.started_at_ms) >= ?
 ORDER BY COALESCE(s.ended_at_ms, s.started_at_ms) DESC
 LIMIT ?`
	rows, err := db.QueryContext(ctx, q, sinceMs, limit)
	if err != nil {
		return nil, fmt.Errorf("query failure shapes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FailureShape
	for rows.Next() {
		var f FailureShape
		if err := rows.Scan(
			&f.SessionID, &f.EndedAtMs, &f.Cwd, &f.Title,
			&f.ToolFailureCount, &f.GitUndoCount, &f.PromptRepeatCount,
			&f.LastEventKind,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// LoadSessionOutcomes returns the outcome rows for the supplied set
// of session IDs, keyed by session_id. Sessions without an outcome
// row are absent from the result; callers handle that as "not yet
// computed."
//
// Empty input returns an empty map (no nil pointer panic), letting
// callers iterate the result without a guard.
func LoadSessionOutcomes(ctx context.Context, db *sql.DB, sessionIDs []string) (map[string]SessionOutcome, error) {
	out := make(map[string]SessionOutcome, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat(",?", len(sessionIDs))[1:]
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		args[i] = id
	}
	q := `SELECT session_id, computed_at_ms,
	             user_prompt_count, tool_use_count, tool_failure_count,
	             error_count, compact_count,
	             git_undo_count, prompt_repeat_count,
	             last_event_kind, outcome
	        FROM session_outcomes
	       WHERE session_id IN (` + placeholders + `)`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("load session_outcomes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var o SessionOutcome
		var label string
		if err := rows.Scan(
			&o.SessionID, &o.ComputedAtMs,
			&o.UserPromptCount, &o.ToolUseCount, &o.ToolFailureCount,
			&o.ErrorCount, &o.CompactCount,
			&o.GitUndoCount, &o.PromptRepeatCount,
			&o.LastEventKind, &label,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		o.Outcome = OutcomeLabel(label)
		out[o.SessionID] = o
	}
	return out, rows.Err()
}

// loadShellCommands returns every kind=shell_command extraction value
// for sessionID, in event-time order. Used by ComputeSessionOutcome
// for git-undo scanning.
func loadShellCommands(ctx context.Context, db *sql.DB, sessionID string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT x.value
		   FROM extractions x
		   JOIN events e ON e.event_id = x.event_id
		  WHERE x.session_id = ? AND x.kind = ?
		  ORDER BY e.ts_source_ms ASC, e.rowid ASC`,
		sessionID, events.ExtractionKindShellCommand)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// loadUserPromptTexts returns the content_text of every user_prompt
// event for sessionID in chronological order. NULL or empty content
// is dropped — repeat detection on absent text would be meaningless.
func loadUserPromptTexts(ctx context.Context, db *sql.DB, sessionID string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT content_text
		   FROM events
		  WHERE session_id = ? AND kind = ?
		    AND content_text IS NOT NULL AND content_text <> ''
		  ORDER BY ts_source_ms ASC, rowid ASC`,
		sessionID, events.KindUserPrompt)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// countGitUndo counts entries in shells whose chain contains AT
// LEAST one command matching gitUndoRE. A `cd repo && git reset
// --hard` line counts once (not twice), matching the natural
// reading "this line is an undo." Multiple genuine undos in one
// chain (rare; e.g. `git stash && git reset --hard`) also count
// once — the signal is per-line, not per-subcommand.
//
// Doesn't try to be a real shell parser — quoted strings containing
// `&&` will fool the splitter, but that's rare in extracted
// shell_command values and the cost of a wrong match is one
// over-counted (or under-counted) git_undo, not data corruption.
func countGitUndo(shells []string) int {
	var n int
	for _, cmd := range shells {
		if slices.ContainsFunc(splitShellChain(cmd), gitUndoRE.MatchString) {
			n++
		}
	}
	return n
}

// splitShellChain splits a shell line on `&&`, `||`, `;` and returns
// every link as a separate string. A line with no chain operator
// returns a one-element slice.
func splitShellChain(line string) []string {
	return shellChainSep.Split(line, -1)
}

// countConsecutiveRepeats counts entries in prompts that are
// byte-equal (after normalisation) to their immediate predecessor.
// Single-occurrence prompts contribute 0; runs of length k contribute
// k-1. Empty/blank entries are skipped (they don't break a run, but
// they also don't extend one).
func countConsecutiveRepeats(prompts []string) int {
	var n int
	prev := ""
	for _, p := range prompts {
		norm := normalizePrompt(p)
		if norm == "" {
			continue
		}
		if prev != "" && norm == prev {
			n++
		}
		prev = norm
	}
	return n
}

// normalizePrompt prepares a user_prompt content_text for byte-equal
// comparison: lowercase, trim outer whitespace, collapse internal
// whitespace runs to a single space.
func normalizePrompt(s string) string {
	return promptWhitespaceRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), " ")
}

// toolFailureFloor returns the minimum tool_failure_count that
// trips OutcomeFailureLikely for a session of the given size. The
// floor stays at 3 for small sessions (≤30 tool attempts) so a
// short failed session is still flagged, but scales linearly above
// that — at 10% of total tool attempts. Without this, a 200-tool
// session with 3 stray failures (1.5% failure rate) would be tagged
// `failure_likely` identically to a 5-tool session with 3 of 5
// failing (60%). The first is normal background noise; the second
// is a broken session.
//
// Called from deriveOutcomeLabel; exposed via unit tests through
// ComputeSessionOutcome.
func toolFailureFloor(toolUseCount, toolFailureCount int) int {
	const baseFloor = 3
	attempts := toolUseCount + toolFailureCount
	scaled := attempts / 10
	if scaled > baseFloor {
		return scaled
	}
	return baseFloor
}

// deriveOutcomeLabel applies the rules documented at the OutcomeLabel
// constants. Pure function over the SessionOutcome counts; called by
// ComputeSessionOutcome and exported (lowercase) for unit-test
// visibility into the rule logic. Test exposure goes through the
// public ComputeSessionOutcome path.
//
// Order matters: failure first (strongest claim), then success
// (clean activity), then unknown (no real activity), else mixed.
func deriveOutcomeLabel(o SessionOutcome) OutcomeLabel {
	endedOnFailure := o.LastEventKind.Valid &&
		(o.LastEventKind.String == events.KindToolFailure ||
			o.LastEventKind.String == events.KindError)

	failFloor := toolFailureFloor(o.ToolUseCount, o.ToolFailureCount)

	switch {
	case o.ToolFailureCount >= failFloor,
		o.GitUndoCount >= 1,
		o.PromptRepeatCount >= 2,
		endedOnFailure && (o.ToolFailureCount+o.ErrorCount) >= 1:
		return OutcomeFailureLikely

	case o.ToolUseCount >= 1 &&
		o.ToolFailureCount == 0 &&
		o.GitUndoCount == 0 &&
		o.PromptRepeatCount == 0 &&
		o.ErrorCount == 0:
		return OutcomeSuccessLikely

	case o.ToolUseCount == 0 && o.UserPromptCount <= 1:
		return OutcomeUnknown

	default:
		return OutcomeMixed
	}
}
