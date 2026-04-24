# Hook payload fixtures

Real Claude Code hook payloads captured via `aichroniclesd` and used as
test inputs for `Assemble`. One file per hook event kind.

## Provenance

Captured by running `aichroniclesd` locally with `aichronicles setup
claude-code` active, then extracting the `.payload` field of each event
from `events.jsonl`. These are verbatim payloads the hook runtime sent
to `aichronicles ingest` on stdin — not synthesized approximations.

## Anonymization

Before committing, every fixture went through:

- `/home/tom/dotfiles`   → `/home/user/project`
- `/home/tom`            → `/home/user`
- `-home-tom-dotfiles`   → `project` (Claude's cwd-slug encoding)
- Gitleaks scan          → passed (no secrets)
- Grep for IPs, API key prefixes (sk-, ghp_, AKIA, Bearer), names,
  emails, hostnames — all clean.

Session IDs (UUIDs) and the `model` string are preserved verbatim
because they carry no personal information.

## Fixtures

| File                     | hook_event_name       | Notes                                |
|--------------------------|-----------------------|--------------------------------------|
| `session_start.json`     | `SessionStart`        | Includes `source: "startup"`, model. |
| `user_prompt.json`       | `UserPromptSubmit`    | Short real prompt.                   |
| `assistant_message.json` | `Stop`                | Full `last_assistant_message` text.  |
| `tool_use_bash.json`     | `PostToolUse`         | `tool_name: Bash` with full response.|
| `tool_use_read.json`     | `PostToolUse`         | `tool_name: Read` with file response.|
| `tool_failure.json`      | `PostToolUseFailure`  | Exit-1 Bash; includes `error` field. |
| `session_end.json`       | `SessionEnd`          | `reason: "other"`.                   |
