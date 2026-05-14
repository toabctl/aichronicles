## aichronicles propose verify

Run the propose-add critic gate without writing anything

### Synopsis

Loads the latest cached propose output (or the one identified
by --output-id), finds the named skill, and runs the Voyager-
style critic LLM against it — the same check `propose add`
runs unless --no-verify is set.

Useful for: previewing the critic's verdict before committing
to an add; debugging a refusal you saw in `propose add`;
warming the kind=propose_verify cache so a subsequent add
is free.

Returns exit code 0 when the critic approves, non-zero with
the concern + recommendation on refusal. Requires ANTHROPIC_API_KEY
on a cache miss.

```
aichronicles propose verify --skill <name> [flags]
```

### Options

```
      --db string       SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help            help for verify
      --output-id int   specific llm_outputs row id (default: latest propose row)
      --skill string    name of a skill from the proposal to verify
      --socket string   aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
```

### SEE ALSO

* [aichronicles propose](./aichronicles_propose.md)	 - LLM-suggested skills / CLAUDE.md entries / scripts from recent sessions
