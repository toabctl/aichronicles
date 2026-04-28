-- Materialise per-session outcome signals.
--
-- aichronicles is a read-only-traces system: it observes Claude Code /
-- Gemini CLI sessions but cannot re-roll them, has no env oracle, and
-- gets no binary reward per task. The literature's smallest viable
-- feedback signal for retrieval-based self-improvement is binary task
-- success (Self-Generated In-Context Examples shows even naive
-- accumulation gives +10–15 absolute points if you can filter for it).
--
-- Without an oracle we settle for proxies — observable patterns in the
-- raw event stream that correlate with "this session did/did not go
-- well." The signals here are intentionally conservative (CLAUDE.md
-- rule 7: correctness over coverage). Each is computed from immutable
-- raw_envelopes / events / extractions, so any row can be deleted and
-- recomputed without information loss.
--
-- Signals captured (raw counts, no thresholds):
--   * user_prompt_count, tool_use_count, tool_failure_count,
--     error_count, compact_count — direct kind aggregates over events
--   * git_undo_count — shell_command extractions matching a narrow set
--     of unambiguous-undo patterns (git reset --hard, git revert,
--     git checkout -- <path>, git restore, git stash push/save). Plain
--     `git reset HEAD` (just unstaging) is deliberately excluded.
--   * prompt_repeat_count — count of consecutive user_prompts whose
--     content_text is byte-for-byte equal after lowercase + whitespace
--     normalisation. False negatives but no false positives.
--   * last_event_kind — the kind of the chronologically last event in
--     the session, useful for "ended on tool_failure" detection
--     without requiring callers to re-query.
--
-- A coarse `outcome` label is derived from the above using rules
-- documented in ComputeSessionOutcome (Go side). Values:
--   * success_likely — clean activity, no failure markers
--   * failure_likely — strong failure signals (failures, undo, repeat)
--   * mixed          — real activity with weak failure signals
--   * unknown        — too thin to label (no tool use, no progress)
--
-- The `_likely` suffix is deliberate: these are heuristics over
-- observational data, not ground truth. Downstream consumers
-- (propose, induction, reflect) read them as priors, not facts.
--
-- Computation is lazy: rows are written by ComputeSessionOutcome /
-- SaveSessionOutcome called by RunPropose and RunInductionForSession
-- before they build their digests. There is no AFTER-INSERT trigger:
-- prompt-repeat detection requires walking user_prompt content_text
-- in chronological order, and git-undo detection requires extraction
-- joins — both too involved for a SQLite trigger. The raw_envelopes
-- invariant (sacred, append-only) means recomputation is always safe.

CREATE TABLE session_outcomes (
    session_id          TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    computed_at_ms      INTEGER NOT NULL,

    -- Raw kind counts.
    user_prompt_count   INTEGER NOT NULL DEFAULT 0,
    tool_use_count      INTEGER NOT NULL DEFAULT 0,
    tool_failure_count  INTEGER NOT NULL DEFAULT 0,
    error_count         INTEGER NOT NULL DEFAULT 0,
    compact_count       INTEGER NOT NULL DEFAULT 0,

    -- Derived signals (require Go-level scan).
    git_undo_count      INTEGER NOT NULL DEFAULT 0,
    prompt_repeat_count INTEGER NOT NULL DEFAULT 0,

    -- Optional shape hint.
    last_event_kind     TEXT,

    -- Coarse derived label (rules in ComputeSessionOutcome).
    outcome             TEXT NOT NULL CHECK(outcome IN
        ('success_likely', 'failure_likely', 'mixed', 'unknown'))
);

-- Index for "find recent failures" / "find recent successes" reads
-- without scanning the whole table. Computed_at_ms is the natural
-- secondary sort for "show me the latest 50 failure_likely sessions".
CREATE INDEX idx_session_outcomes_outcome ON session_outcomes(outcome, computed_at_ms DESC);

UPDATE meta SET value='17' WHERE key='schema_version';
