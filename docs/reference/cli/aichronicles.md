## aichronicles

Capture AI coding agent session events

### Synopsis

aichronicles is the client binary for the aichroniclesd ingest daemon. It receives hook payloads, wraps them in the canonical Envelope, and forwards to the daemon over a Unix domain socket.

### Options

```
  -h, --help   help for aichronicles
```

### SEE ALSO

* [aichronicles audit](./aichronicles_audit.md)	 - Scan stored events for credential patterns (read-only)
* [aichronicles import-claude](./aichronicles_import-claude.md)	 - Import Claude Code's own ~/.claude transcripts into the store
* [aichronicles import-jsonl](./aichronicles_import-jsonl.md)	 - Replay events.jsonl into the SQLite store
* [aichronicles ingest](./aichronicles_ingest.md)	 - Read a hook payload on stdin and forward as an envelope
* [aichronicles mcp-serve](./aichronicles_mcp-serve.md)	 - Run an MCP server over stdio exposing aichronicles data
* [aichronicles propose](./aichronicles_propose.md)	 - LLM-suggested skills / CLAUDE.md entries / scripts from recent sessions
* [aichronicles reflect](./aichronicles_reflect.md)	 - LLM-derived meta-analysis of recent sessions
* [aichronicles scrub](./aichronicles_scrub.md)	 - Rewrite stored events to remove credentials (IRREVERSIBLE with --yes)
* [aichronicles search](./aichronicles_search.md)	 - Full-text search over captured envelopes
* [aichronicles sessions](./aichronicles_sessions.md)	 - List sessions in the store, most-recently-ended first
* [aichronicles setup](./aichronicles_setup.md)	 - Install aichronicles into an AI coding agent or the OS
* [aichronicles summaries](./aichronicles_summaries.md)	 - Inspect stored LLM outputs (summaries, reflections, proposals)
* [aichronicles summarize](./aichronicles_summarize.md)	 - Generate an LLM summary for one session
* [aichronicles teardown](./aichronicles_teardown.md)	 - Remove aichronicles integration from an AI coding agent or the OS
* [aichronicles version](./aichronicles_version.md)	 - Print build version, commit, and Go toolchain
