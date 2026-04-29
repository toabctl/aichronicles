## aichronicles induction sweep

Walk recently-ended sessions and run the auto-extraction pipeline on each

### Synopsis

Selects sessions that (a) have an ended_at_ms older than
--idle, (b) have at least --min-events recorded events,
and (c) haven't been induced before, then runs the per-
session pipeline on each:

  phase 1: summarize       (when no kind=summary row exists)
  phase 2: induction       (skill + workflow merged)
  phase 3: facts           (typed semantic facts)

Each phase is cache-idempotent on prompt-hash — re-running
the sweep skips sessions whose rows already exist. Phase 1
failure SKIPS phases 2+3 (they require a summary); phases
2 and 3 are independent and run even if the other failed.

Designed to be triggered from the daemon's resident
sweeper. The CLI prints a per-session line so the
operator can tell which session yielded a skill, a
workflow, both, or nothing.

Pass --skip-summarize to keep summarize manual (sessions
without summaries will then bail with their existing 'no
summary' error in phase 2). Pass --skip-facts to suppress
the facts induction call. Either flag halves per-
candidate spend.

Requires ANTHROPIC_API_KEY.

```
aichronicles induction sweep [flags]
```

### Options

```
      --db string        SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help             help for sweep
      --idle duration    only consider sessions with no events for this long (e.g. 15m, 1h) (default 30m0s)
      --limit int        max sessions to process in one sweep (default 25)
      --min-events int   skip sessions with fewer than this many events (default 5)
      --model string     LLM model id (default: provider's default)
      --skip-episodes    skip phase 0 (episode segmentation); episode-keyed retrieval will have no rows for new candidates
      --skip-facts       skip phase 3 (semantic-facts induction); saves one LLM call per candidate
      --skip-summarize   skip phase 1 (auto-summarize). Sessions without summaries will be skipped — keeps summarize manual.
```

### SEE ALSO

* [aichronicles induction](./aichronicles_induction.md)	 - Online single-session induction (AWM-style auto-skill-extraction)
