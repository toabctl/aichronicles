## aichronicles setup codex-cli

Install Codex CLI hooks that forward events to aichronicles-api

### Synopsis

Idempotently merges five hook entries (UserPromptSubmit, Stop,
PostToolUse, SessionStart, SessionEnd) into Codex's hooks.json,
each pointing at `aichronicles hook --agent codex-cli`.
Existing hook entries from other tools are preserved; running
twice is a no-op.

Default path is $CODEX_HOME/hooks.json, or ~/.codex/hooks.json
when CODEX_HOME is unset. Pass --settings to target a
project-local <repo>/.codex/hooks.json instead.

INSTALL AT ONE LAYER ONLY. Codex merges its hook layers (user,
project, plugin, managed) rather than letting the nearest one
win, so an entry in both ~/.codex/hooks.json and a repo's
.codex/hooks.json fires our hook twice and stores every event
of that session twice. Codex also accepts hooks inline as
[hooks] in config.toml; we neither read nor write that form,
so a hand-written inline entry is a second layer too.

Codex's hook protocol is a clone of Claude Code's, down to the
PascalCase event names and the tool vocabulary (a shell call
arrives as tool_name="Bash"), so the same translator shape
handles it. It has no tool-failure event and no error channel
on tool_response, so every PostToolUse is recorded as a plain
tool_use.

The SessionEnd entry carries an explicit `timeout` of 3s —
Codex defaults that one event to 1s (every other event gets
600s) and caps it at 3s, which is tight enough that a busy
machine can lose the event.

ONE MANUAL STEP REMAINS: Codex will not run a hook command it
has not been told to trust. The next interactive `codex` run
prompts you to review and trust the new entries — or run
/hooks inside Codex to review them on demand. Until you
accept, nothing is captured.

```
aichronicles setup codex-cli [flags]
```

### Options

```
      --command string    command to run from each hook (default "aichronicles hook --agent codex-cli")
  -h, --help              help for codex-cli
      --settings string   path to Codex hooks.json (default: $CODEX_HOME/hooks.json, else ~/.codex/hooks.json)
```

### SEE ALSO

* [aichronicles setup](./aichronicles_setup.md)	 - Install aichronicles into an AI coding agent or the OS
