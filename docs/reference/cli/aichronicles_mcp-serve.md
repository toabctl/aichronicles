## aichronicles mcp-serve

Run an MCP server over stdio exposing aichronicles data

### Synopsis

Starts a Model Context Protocol server on stdin/stdout,
offering three read-only tools (search_events, list_sessions,
get_summary) backed by the local SQLite store.

Typically registered in Claude Desktop's mcp_servers section:

    "aichronicles": {
      "command": "/home/you/.local/bin/aichronicles",
      "args": ["mcp-serve"]
    }

Logs are emitted as structured records on stderr so Claude
Desktop's own log window surfaces them. Stdin close (client
disconnect) ends the process cleanly.

```
aichronicles mcp-serve [flags]
```

### Options

```
      --db string   SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)
  -h, --help        help for mcp-serve
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
