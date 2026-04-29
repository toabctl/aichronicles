## aichronicles induction

Online single-session induction (AWM-style auto-skill-extraction)

### Synopsis

When a session ends, induction asks the LLM whether the
trajectory contained ONE concrete reusable workflow worth
saving as a Claude Code skill. The bar is high — the model
is told to default to no_skill_found unless it can name a
specific trigger condition the user is likely to hit again.

Subcommands:
  sweep — walk recently-ended sessions and induce on each
  run   — induce on one specific session id
  list  — show induction outcomes recorded so far


### Options

```
  -h, --help   help for induction
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
* [aichronicles induction list](./aichronicles_induction_list.md)	 - Show recent induction runs (proposed skills, no_skill_found verdicts)
* [aichronicles induction run](./aichronicles_induction_run.md)	 - Induce on one specific session id
* [aichronicles induction sweep](./aichronicles_induction_sweep.md)	 - Walk recently-ended sessions and run the auto-extraction pipeline on each
