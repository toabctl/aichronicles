package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
)

// nullS is a tiny events.NullString constructor for table-driven
// tests that need to populate Cwd / Role values on events.EventView
// fixtures. events.Episode.Cwd uses sql.NullString — for those tests, see
// nullSQL.
func nullS(s string) events.NullString {
	return events.NullString{String: s, Valid: s != ""}
}

// TestSegmentSession_EmptyInput pins the trivial cases — empty
// session id and empty event slice both return nil.
func TestSegmentSession_EmptyInput(t *testing.T) {
	t.Parallel()
	if got := SegmentSession("", nil, 0); got != nil {
		t.Errorf("empty inputs: got %#v, want nil", got)
	}
	if got := SegmentSession("sess", nil, 0); got != nil {
		t.Errorf("nil events: got %#v, want nil", got)
	}
}

// TestSegmentSession_SingleEpisode covers the boring path: a
// session whose events all sit within idleGapMs of each other and
// share one cwd produces exactly one episode covering everything.
func TestSegmentSession_SingleEpisode(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{EventID: "e1", Kind: events.KindUserPrompt,
			ContentText: nullS("how do I fix the build"),
			Cwd:         nullS("/repo/a"), TsSourceMs: 1_000},
		{EventID: "e2", Kind: events.KindToolUse, Cwd: nullS("/repo/a"), TsSourceMs: 2_000},
		{EventID: "e3", Kind: events.KindToolResult, Cwd: nullS("/repo/a"), TsSourceMs: 3_000},
		{EventID: "e4", Kind: events.KindAssistantMessage, Cwd: nullS("/repo/a"), TsSourceMs: 4_000},
	}
	got := SegmentSession("sess-single", events, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 episode, got %d: %+v", len(got), got)
	}
	ep := got[0]
	if ep.Ordinal != 1 {
		t.Errorf("ordinal: got %d want 1", ep.Ordinal)
	}
	if ep.StartedAtMs != 1_000 || ep.EndedAtMs != 4_000 {
		t.Errorf("time bracket: got [%d,%d] want [1000,4000]", ep.StartedAtMs, ep.EndedAtMs)
	}
	if ep.EventCount != 4 {
		t.Errorf("event_count: got %d want 4", ep.EventCount)
	}
	if !ep.Cwd.Valid || ep.Cwd.String != "/repo/a" {
		t.Errorf("cwd: got %v want /repo/a", ep.Cwd)
	}
	if !strings.Contains(ep.IntentSummary, "how do I fix the build") {
		t.Errorf("intent: got %q", ep.IntentSummary)
	}
	if ep.FirstEventID != "e1" {
		t.Errorf("first_event_id: got %q want e1", ep.FirstEventID)
	}
}

// TestSegmentSession_IdleGapBoundary covers the canonical
// segmentation trigger: an inter-event gap ≥ idleGapMs splits
// the session into two episodes.
func TestSegmentSession_IdleGapBoundary(t *testing.T) {
	t.Parallel()
	const gap = int64(10_000) // 10s for test brevity
	events := []events.EventView{
		{EventID: "a", Kind: events.KindUserPrompt, ContentText: nullS("first intent"), TsSourceMs: 0},
		{EventID: "b", Kind: events.KindAssistantMessage, TsSourceMs: 1_000},
		// 12s gap → episode boundary.
		{EventID: "c", Kind: events.KindUserPrompt, ContentText: nullS("second intent"), TsSourceMs: 13_000},
		{EventID: "d", Kind: events.KindAssistantMessage, TsSourceMs: 14_000},
	}
	got := SegmentSession("sess-gap", events, gap)
	if len(got) != 2 {
		t.Fatalf("expected 2 episodes, got %d: %+v", len(got), got)
	}
	if got[0].Ordinal != 1 || got[1].Ordinal != 2 {
		t.Errorf("ordinals: got %d / %d", got[0].Ordinal, got[1].Ordinal)
	}
	if got[0].EndedAtMs != 1_000 || got[1].StartedAtMs != 13_000 {
		t.Errorf("boundary: got ep1.end=%d ep2.start=%d", got[0].EndedAtMs, got[1].StartedAtMs)
	}
	if !strings.Contains(got[0].IntentSummary, "first intent") {
		t.Errorf("ep1 intent: got %q", got[0].IntentSummary)
	}
	if !strings.Contains(got[1].IntentSummary, "second intent") {
		t.Errorf("ep2 intent: got %q", got[1].IntentSummary)
	}
}

// TestSegmentSession_CwdShiftBoundary covers the second trigger:
// a cwd change closes the running episode even if the events are
// time-adjacent. Catches the "user cd-hopped between projects in
// the same session" pattern.
func TestSegmentSession_CwdShiftBoundary(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{EventID: "x", Kind: events.KindUserPrompt, Cwd: nullS("/repo/a"), ContentText: nullS("project A work"), TsSourceMs: 0},
		{EventID: "y", Kind: events.KindToolUse, Cwd: nullS("/repo/a"), TsSourceMs: 100},
		// Same time-frame, but cwd shifted — still a boundary.
		{EventID: "z", Kind: events.KindUserPrompt, Cwd: nullS("/repo/b"), ContentText: nullS("project B work"), TsSourceMs: 200},
	}
	got := SegmentSession("sess-cwd", events, 0)
	if len(got) != 2 {
		t.Fatalf("expected 2 episodes, got %d: %+v", len(got), got)
	}
	if got[0].Cwd.String != "/repo/a" || got[1].Cwd.String != "/repo/b" {
		t.Errorf("cwds: got [%s,%s]", got[0].Cwd.String, got[1].Cwd.String)
	}
}

// TestSegmentSession_NullCwdDoesNotTrigger asserts that an event
// with NULL cwd doesn't spuriously close the episode — a tool that
// happens to omit cwd shouldn't fragment the timeline.
func TestSegmentSession_NullCwdDoesNotTrigger(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{EventID: "p", Kind: events.KindUserPrompt, Cwd: nullS("/repo/a"), TsSourceMs: 0},
		{EventID: "q", Kind: events.KindToolUse /* no cwd */, TsSourceMs: 100},
		{EventID: "r", Kind: events.KindToolResult, Cwd: nullS("/repo/a"), TsSourceMs: 200},
	}
	got := SegmentSession("sess-nullcwd", events, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 episode, got %d: %+v", len(got), got)
	}
	if got[0].EventCount != 3 {
		t.Errorf("event_count: got %d want 3", got[0].EventCount)
	}
}

// TestClipIntentSummary covers the intent-summary cap: long bodies
// truncate with an ellipsis; embedded newlines collapse to spaces.
func TestClipIntentSummary(t *testing.T) {
	t.Parallel()
	short := "fix the build"
	if got := clipIntentSummary(short); got != short {
		t.Errorf("short: got %q want %q", got, short)
	}
	multiline := "fix\nthe\nbuild"
	if got := clipIntentSummary(multiline); got != "fix the build" {
		t.Errorf("multiline collapse: got %q", got)
	}
	long := strings.Repeat("x", MaxEpisodeIntentSummaryRunes+50)
	got := clipIntentSummary(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis on overflow")
	}
	if len([]rune(got)) > MaxEpisodeIntentSummaryRunes+1 {
		t.Errorf("overflow truncate failed: %d runes", len([]rune(got)))
	}
}

// seedEpisodeRow inserts one episode row directly so the
// FindEpisodes table-driven tests below can build a multi-episode
// session without tripping SaveEpisodes' DELETE-then-INSERT
// semantics (which would wipe earlier rows on each call). We
// exercise the QUERY surface, not the segmentation algorithm —
// building events fixtures for every case would obscure what's
// under test.
func seedEpisodeRow(t *testing.T, s *Store, sessionID string, ord int, startMs, endMs int64, cwd, intent string) {
	t.Helper()
	if _, err := s.DB().Exec(
		`INSERT OR IGNORE INTO sessions(id, source_agent, source_session_id) VALUES (?, ?, ?)`,
		sessionID, "claude-code", "src-"+sessionID,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	// Plant one envelope+event so first_event_id has a real referent
	// (the FK in the episodes schema needs an actual row).
	eid := mkUUIDLikeID(t, "evt-"+sessionID, ord)
	if _, err := s.DB().Exec(
		`INSERT OR IGNORE INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES (?, ?, ?, ?, ?, ?, '{}')`,
		eid, time.Now().UnixNano()+int64(ord), "claude-code", "src-"+sessionID, startMs, startMs,
	); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT OR IGNORE INTO events(event_id, session_id, source_agent, kind, ts_source_ms, content_text, cwd)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		eid, sessionID, "claude-code", "user_prompt", startMs, intent, cwd,
	); err != nil {
		t.Fatalf("event: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO episodes(session_id, ordinal, started_at_ms, ended_at_ms,
			cwd, intent_summary, event_count, first_event_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, ord, startMs, endMs, cwd, intent, 1, eid,
	); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
}

// TestFindEpisodes_Filters covers each filter dimension on an
// otherwise-shared corpus: 4 episodes across 3 sessions in 2 cwds.
func TestFindEpisodes_Filters(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	// Avoid relying on time.Now() so the SinceMs slice is
	// deterministic across runs.
	const (
		sess1 = "00000000-0000-0000-0000-00000000aaaa"
		sess2 = "00000000-0000-0000-0000-00000000bbbb"
		sess3 = "00000000-0000-0000-0000-00000000cccc"
	)
	seedEpisodeRow(t, s, sess1, 1, 1_000, 2_000, "/repo/a", "fix the build script")
	seedEpisodeRow(t, s, sess2, 1, 3_000, 4_000, "/repo/a", "review staging deploy")
	seedEpisodeRow(t, s, sess2, 2, 5_000, 6_000, "/repo/b", "explore the new module")
	seedEpisodeRow(t, s, sess3, 1, 7_000, 8_000, "/repo/b", "FIX the FLAKY test")

	// All four episodes — bare opts.
	all, err := FindEpisodes(ctx, s.DB(), FindEpisodesOpts{})
	if err != nil {
		t.Fatalf("FindEpisodes(zero): %v", err)
	}
	if len(all) != 4 {
		t.Errorf("zero opts: got %d episodes, want 4", len(all))
	}
	// Result is ORDER BY ended_at_ms DESC — episode at ts 8_000 first.
	if all[0].EndedAtMs != 8_000 {
		t.Errorf("ordering: got first.EndedAtMs=%d, want 8000 (most-recent)", all[0].EndedAtMs)
	}

	// Cwd filter: /repo/a → 2 hits.
	cwdHits, err := FindEpisodes(ctx, s.DB(), FindEpisodesOpts{Cwd: "/repo/a"})
	if err != nil {
		t.Fatalf("FindEpisodes(cwd): %v", err)
	}
	if len(cwdHits) != 2 {
		t.Errorf("cwd filter: got %d, want 2", len(cwdHits))
	}
	for _, ep := range cwdHits {
		if ep.Cwd.String != "/repo/a" {
			t.Errorf("cwd filter leaked %q", ep.Cwd.String)
		}
	}

	// SessionID filter: sess2 → 2 episodes.
	sessHits, err := FindEpisodes(ctx, s.DB(), FindEpisodesOpts{SessionID: sess2})
	if err != nil {
		t.Fatalf("FindEpisodes(session): %v", err)
	}
	if len(sessHits) != 2 {
		t.Errorf("session filter: got %d, want 2", len(sessHits))
	}

	// SinceMs filter: only episodes ended at or after 5_000 → 2.
	recent, err := FindEpisodes(ctx, s.DB(), FindEpisodesOpts{SinceMs: 5_000})
	if err != nil {
		t.Fatalf("FindEpisodes(since): %v", err)
	}
	if len(recent) != 2 {
		t.Errorf("since filter: got %d, want 2 (ended_at>=5000)", len(recent))
	}

	// QueryContains: "fix" should match both "fix the build script"
	// and "FIX the FLAKY test" (case-insensitive).
	fixHits, err := FindEpisodes(ctx, s.DB(), FindEpisodesOpts{QueryContains: "fix"})
	if err != nil {
		t.Fatalf("FindEpisodes(query): %v", err)
	}
	if len(fixHits) != 2 {
		t.Errorf("query filter: got %d, want 2", len(fixHits))
	}

	// Combined: cwd=/repo/b AND query=flaky → 1.
	combined, err := FindEpisodes(ctx, s.DB(), FindEpisodesOpts{
		Cwd:           "/repo/b",
		QueryContains: "flaky",
	})
	if err != nil {
		t.Fatalf("FindEpisodes(combined): %v", err)
	}
	if len(combined) != 1 {
		t.Errorf("combined filter: got %d, want 1", len(combined))
	}

	// Limit: capped to 1 → 1 returned, the most recent.
	capped, err := FindEpisodes(ctx, s.DB(), FindEpisodesOpts{Limit: 1})
	if err != nil {
		t.Fatalf("FindEpisodes(limit): %v", err)
	}
	if len(capped) != 1 || capped[0].EndedAtMs != 8_000 {
		t.Errorf("limit: got %+v, want one most-recent episode", capped)
	}
}

// TestEpisodesIndex_Migration026Applied pins migration 026's
// outcome: the started-anchored partial index is gone and the
// ended-anchored one is in place. Without this, a future revert of
// 026 would silently re-introduce the materialise-and-sort hot path
// the migration removes.
func TestEpisodesIndex_Migration026Applied(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	rows, err := s.DB().Query(
		`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='episodes'`,
	)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		have[n] = true
	}
	if have["idx_episodes_cwd_started"] {
		t.Errorf("migration 026 should have dropped idx_episodes_cwd_started")
	}
	if !have["idx_episodes_cwd_ended"] {
		t.Errorf("migration 026 should have created idx_episodes_cwd_ended (have=%v)", have)
	}
}

// TestFindEpisodes_TiebreakerOnEndedAt pins the deterministic order
// when two episodes share an ended_at_ms (typical for
// single-event-bracket episodes pinned to the same wall clock, or
// fixtures with hardcoded ts). Without `, id DESC` the engine is
// free to return either order, which surfaces as flaky LIMIT'd
// recall and intermittent CI in tests that assume newest-first.
func TestFindEpisodes_TiebreakerOnEndedAt(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	// Two episodes with identical ended_at_ms in two different
	// sessions. The newer-inserted row (higher autoincrement id)
	// must come first.
	const tied = int64(5_000)
	seedEpisodeRow(t, s, "00000000-0000-0000-0000-00000000ee01", 1, 1_000, tied, "/repo/x", "earlier insert")
	seedEpisodeRow(t, s, "00000000-0000-0000-0000-00000000ee02", 1, 2_000, tied, "/repo/x", "later insert")

	for i := range 10 {
		hits, err := FindEpisodes(ctx, s.DB(), FindEpisodesOpts{})
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if len(hits) != 2 {
			t.Fatalf("iter %d: got %d hits, want 2", i, len(hits))
		}
		if hits[0].IntentSummary != "later insert" {
			t.Errorf("iter %d: tiebreaker violated; first.intent=%q (want %q)",
				i, hits[0].IntentSummary, "later insert")
		}
	}
}

// TestFindEpisodes_QueryContainsTrimsWhitespace pins the
// trim-then-empty behaviour: a query that's pure whitespace must NOT
// generate a SQL filter (otherwise '%   %' would match every row,
// which is silly but harmless — the bug we're guarding against is
// "the agent passed an unintentional space and the query degraded
// to no-op gracefully").
func TestFindEpisodes_QueryContainsTrimsWhitespace(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	seedEpisodeRow(t, s, "00000000-0000-0000-0000-00000000ddd1", 1, 1, 2, "/repo/x", "anything")

	hits, err := FindEpisodes(ctx, s.DB(), FindEpisodesOpts{QueryContains: "   "})
	if err != nil {
		t.Fatalf("FindEpisodes: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("whitespace-only query should be a no-op filter, got %d", len(hits))
	}
}

// TestLoadSessionsNeedingSegmentation pins the staleness rule the
// daemon uses to decide which sessions to (re-)segment. Pre-fix,
// segmentation lived inside the per-candidate induction loop, so
// once a session was inducted it dropped out forever — late
// events arriving after induction created silent episode/event
// drift. The new pass keys on episode lag directly.
func TestLoadSessionsNeedingSegmentation(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	const (
		now      = int64(2_000_000)
		idleMs   = int64(60_000)
		minEvent = 3
	)
	idleCutoff := now // sessions ending at or before "now-idleMs" qualify

	// Session A: 5 events, no episodes yet → MUST segment.
	const sessA = "00000000-0000-0000-0000-00000000aa01"
	seedSessionWithEvents(t, s, sessA, 5, now-2*idleMs)

	// Session B: 5 events, fully covered by one episode → MUST NOT segment.
	const sessB = "00000000-0000-0000-0000-00000000bb02"
	seedSessionWithEvents(t, s, sessB, 5, now-2*idleMs)
	if _, err := s.DB().Exec(
		`INSERT INTO episodes(session_id, ordinal, started_at_ms, ended_at_ms,
			cwd, intent_summary, event_count, first_event_id)
		 VALUES (?, 1, ?, ?, NULL, '', 5, (SELECT event_id FROM events WHERE session_id=? ORDER BY ts_source_ms LIMIT 1))`,
		sessB, now-2*idleMs, now-2*idleMs+1000, sessB,
	); err != nil {
		t.Fatalf("seed sessB episode: %v", err)
	}

	// Session C: 5 events; episodes ended at ts=A; an event exists with
	// ts > episode.ended_at → MUST segment (new events past the last
	// episode).
	const sessC = "00000000-0000-0000-0000-00000000cc03"
	seedSessionWithEvents(t, s, sessC, 5, now-2*idleMs)
	if _, err := s.DB().Exec(
		`INSERT INTO episodes(session_id, ordinal, started_at_ms, ended_at_ms,
			cwd, intent_summary, event_count, first_event_id)
		 VALUES (?, 1, ?, ?, NULL, '', 1, (SELECT event_id FROM events WHERE session_id=? ORDER BY ts_source_ms LIMIT 1))`,
		// the episode covers only the first event; events 2..5 are
		// "newer than max(episodes.ended_at_ms)" → re-segmentation needed.
		sessC, now-2*idleMs, now-2*idleMs, sessC,
	); err != nil {
		t.Fatalf("seed sessC episode: %v", err)
	}

	// Session D: too few events (below minEvents) → MUST NOT segment.
	const sessD = "00000000-0000-0000-0000-00000000dd04"
	seedSessionWithEvents(t, s, sessD, 2, now-2*idleMs)

	// Session E: not idle yet (ended too recently) → MUST NOT segment.
	const sessE = "00000000-0000-0000-0000-00000000ee05"
	seedSessionWithEvents(t, s, sessE, 5, now-1) // ended 1ms ago

	got, err := LoadSessionsNeedingSegmentation(ctx, s.DB(), idleCutoff, idleMs, minEvent, 50)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	gotSet := map[string]bool{}
	for _, id := range got {
		gotSet[id] = true
	}
	if !gotSet[sessA] {
		t.Errorf("sessA (no episodes) should be returned; got %v", got)
	}
	if !gotSet[sessC] {
		t.Errorf("sessC (events past last episode) should be returned; got %v", got)
	}
	if gotSet[sessB] {
		t.Errorf("sessB (fully covered) should NOT be returned; got %v", got)
	}
	if gotSet[sessD] {
		t.Errorf("sessD (below min events) should NOT be returned; got %v", got)
	}
	if gotSet[sessE] {
		t.Errorf("sessE (not idle) should NOT be returned; got %v", got)
	}
}

// seedSessionWithEvents inserts a sessions row, raw_envelopes, and
// `count` events with ts_source_ms starting at startMs and stepping
// 1ms apart. Used by TestLoadSessionsNeedingSegmentation to build
// the corpus.
//
// The sessions row leaves event_count at the schema default (0)
// and lets the AFTER INSERT events trigger increment it once per
// event — that way the final event_count exactly matches `count`
// without having to anticipate the trigger.
func seedSessionWithEvents(t *testing.T, s *Store, sessionID string, count int, startMs int64) {
	t.Helper()
	if _, err := s.DB().Exec(
		`INSERT OR IGNORE INTO sessions(id, source_agent, source_session_id,
		      started_at_ms, ended_at_ms)
		 VALUES (?, 'claude-code', ?, ?, ?)`,
		sessionID, "src-"+sessionID, startMs, startMs+int64(count),
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for i := range count {
		eid := mkUUIDLikeID(t, "evt-"+sessionID, i)
		if _, err := s.DB().Exec(
			`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id,
			      ts_source_ms, ts_server_ms, envelope_json)
			 VALUES (?, ?, ?, ?, ?, ?, '{}')`,
			eid, time.Now().UnixNano()+int64(i), "claude-code", "src-"+sessionID, startMs+int64(i), startMs+int64(i),
		); err != nil {
			t.Fatalf("envelope: %v", err)
		}
		if _, err := s.DB().Exec(
			`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms)
			 VALUES (?, ?, 'claude-code', 'user_prompt', ?)`,
			eid, sessionID, startMs+int64(i),
		); err != nil {
			t.Fatalf("event: %v", err)
		}
	}
}

// TestSaveAndLoadEpisodes covers the round-trip via a real session.
// The fixture seeds raw_envelopes + events for one session, runs
// the segmenter, persists, and reloads — every events.Episode field must
// round-trip exactly.
func TestSaveAndLoadEpisodes(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	// Seed minimal sessions row + 4 events, two of which sit on
	// either side of an idle-gap boundary so the segmenter
	// produces 2 episodes.
	const sessID = "00000000-0000-0000-0000-0000000000ab"
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, ?, ?)`,
		sessID, "claude-code", "src-ab",
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for i, ts := range []int64{0, 1_000, 1_000_000_000, 1_000_001_000} {
		eid := mkUUIDLikeID(t, "evt", i)
		if _, err := s.DB().Exec(
			`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
			 VALUES (?, ?, ?, ?, ?, ?, '{}')`,
			eid, i+1, "claude-code", "src-ab", ts, ts,
		); err != nil {
			t.Fatalf("envelope: %v", err)
		}
		if _, err := s.DB().Exec(
			`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, content_text, cwd)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			eid, sessID, "claude-code", events.KindUserPrompt, ts, "intent text", "/repo/a",
		); err != nil {
			t.Fatalf("event: %v", err)
		}
	}

	events, err := LoadEventsForSession(ctx, s.DB(), sessID, 0)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("seeded events: got %d want 4", len(events))
	}

	// Use 60s idle gap so the 1_000_000s gap (≈ 17min) fires.
	episodes := SegmentSession(sessID, events, 60_000)
	if len(episodes) != 2 {
		t.Fatalf("segmenter: got %d episodes, want 2", len(episodes))
	}

	n, err := SaveEpisodes(ctx, s.DB(), sessID, episodes)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if n != 2 {
		t.Errorf("inserted: got %d want 2", n)
	}

	loaded, err := LoadEpisodesBySession(ctx, s.DB(), sessID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded: got %d want 2", len(loaded))
	}
	if loaded[0].Ordinal != 1 || loaded[1].Ordinal != 2 {
		t.Errorf("ordinals: got %d / %d", loaded[0].Ordinal, loaded[1].Ordinal)
	}
	if loaded[0].SessionID != sessID {
		t.Errorf("session_id round-trip: got %q", loaded[0].SessionID)
	}
	if loaded[0].FirstEventID == "" {
		t.Errorf("first_event_id missing")
	}

	// Re-saving must be idempotent (DELETE-then-INSERT semantics).
	n2, err := SaveEpisodes(ctx, s.DB(), sessID, episodes)
	if err != nil {
		t.Fatalf("re-save: %v", err)
	}
	if n2 != 2 {
		t.Errorf("re-insert count: got %d want 2", n2)
	}
	reloaded, _ := LoadEpisodesBySession(ctx, s.DB(), sessID)
	if len(reloaded) != 2 {
		t.Errorf("re-load len: got %d want 2", len(reloaded))
	}
}
