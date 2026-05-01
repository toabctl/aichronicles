## aichronicles meta

Cadence-gated meta-analyses (propose / reflect / challenge / reflect_weekly / skill_revision)

### Synopsis

meta is the umbrella for time-driven analyses that fire
on a per-kind cadence rather than per-session. The cadence
gate runs against the persisted last-fired timestamp in
SQLite (kind=propose, kind=reflect, etc.), so a missed
timer firing is automatically picked up on the next run.

Subcommands:
  sweep — fire every overdue kind in one pass


### Options

```
  -h, --help   help for meta
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
* [aichronicles meta sweep](./aichronicles_meta_sweep.md)	 - Fire every overdue meta-analysis kind in one pass
