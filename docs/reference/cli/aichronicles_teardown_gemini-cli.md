## aichronicles teardown gemini-cli

Remove aichronicles Gemini CLI hooks from settings.json

### Synopsis

Inverse of `setup gemini-cli`. Strips every hook entry whose
command matches ours from each Gemini hook event. Other tools'
entries are preserved; running twice is a no-op.

Dry-run by default: pass --yes to actually rewrite the file.

```
aichronicles teardown gemini-cli [flags]
```

### Options

```
      --command string    command to strip from each hook (default "aichronicles ingest --agent gemini-cli")
  -h, --help              help for gemini-cli
      --settings string   path to Gemini settings.json (default: ~/.gemini/settings.json)
      --yes               confirm the removal (required to modify the file)
```

### SEE ALSO

* [aichronicles teardown](./aichronicles_teardown.md)	 - Remove aichronicles integration from an AI coding agent or the OS
