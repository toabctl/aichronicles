## aichronicles import-gemini

Import gemini-cli session JSON files into the store

### Synopsis

Walks one or more gemini-cli session files (one JSON per
conversation, written under ~/.gemini/tmp/<project>/chats/) and
ingests every message — user prompt, assistant turn, tool call,
tool result — as a canonical envelope.

event_id is the message's own UUID for user / assistant turns;
tool_use and tool_result events synthesize an id by
UUIDv5(namespace, parentMessageID + tool_call_id + suffix). All
three are stable across re-imports so the operation is
idempotent.

path defaults to ~/.gemini/tmp (gemini-cli's per-project root).
A specific session-*.json file or any directory below the root
works too.

Trust model: like import-claude, this bypasses the daemon. Edge
redaction runs in-process; anything else the daemon enforces
does not.

```
aichronicles import-gemini [path] [flags]
```

### Options

```
      --db string   SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)
  -h, --help        help for import-gemini
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
