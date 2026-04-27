## aichronicles digest

Periodic LLM-driven digests stored as queryable artefacts

### Synopsis

Runs reflect over a fixed time window and writes the result
into llm_outputs as a tagged artefact (kind=reflect_weekly).
Unlike ad-hoc `reflect`, the body is wrapped with
period_start/period_end metadata so the result is queryable
as a timeline of past weeks (via `digest list`).

Designed to be cron-friendly: rerunning for the same week is
a cache hit on prompt_hash; --force re-calls the LLM.

### Options

```
  -h, --help   help for digest
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
* [aichronicles digest list](./aichronicles_digest_list.md)	 - List past weekly digest artefacts (kind=reflect_weekly)
* [aichronicles digest weekly](./aichronicles_digest_weekly.md)	 - Generate a weekly reflect digest, persisted with kind=reflect_weekly
