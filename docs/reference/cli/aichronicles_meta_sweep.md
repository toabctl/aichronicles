## aichronicles meta sweep

Fire every overdue meta-analysis kind in one pass

### Synopsis

Walks the cadence-gated kinds (propose / reflect /
challenge / reflect_weekly / skill_revision) and dispatches
any whose persisted last-fired timestamp is older than the
configured cadence. Cadences and per-kind skip flags come
from [meta_analysis] in the config file; defaults match
the prompts' natural horizons (24h / 7d).

Per-kind failure isolation: a propose failure does not
skip the week's reflect digest. The first non-nil error
is returned, but every eligible kind is attempted before
the command exits.

Designed to be driven from a systemd --user timer (see
the install assets). Manual invocation works too — useful
when forcing one round outside the cadence.

Requires ANTHROPIC_API_KEY.

```
aichronicles meta sweep [flags]
```

### Options

```
      --db string   SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help        help for sweep
```

### SEE ALSO

* [aichronicles meta](./aichronicles_meta.md)	 - Cadence-gated meta-analyses (propose / reflect / challenge / reflect_weekly / skill_revision)
