# Claude transcript fixtures

Synthetic `.jsonl` lines mirroring the shape of Claude Code's own
session transcripts at `~/.claude/projects/<encoded-cwd>/*.jsonl`.

## Why synthetic, not captured

The hook fixtures under `../hooks/` were captured live with real
session UUIDs retained, which on reflection leaks more detail than
needed. For these transcript fixtures every UUID and session id is a
deliberately synthetic repeating-digit pattern (`11111111-…`,
`22222222-…`) so there is no correlation with any real session on
any machine. Content is also synthetic ("write me a hello world",
etc.), not captured prose.

## Files

| File | Shape | Exercises |
|---|---|---|
| `user_prompt_string.jsonl` | user type, content is a bare string | `user_prompt` classification |
| `user_tool_result.jsonl` | user type, content is array containing a `tool_result` block | `tool_result` classification, `tool.call_id` extraction |
| `assistant_text.jsonl` | assistant type, text-only content | `assistant_message` classification, text flattening |
| `assistant_tool_use.jsonl` | assistant type, content contains a `tool_use` block | `tool_use` classification, `tool.name` + `tool.call_id` extraction |
| `system.jsonl` | system type | `system_message` classification |
| `mixed_session.jsonl` | One session's line sequence mixing canonical + internal + a missing-uuid row | file-level import: skip policy, missing-uuid loudness, end-to-end counts |
