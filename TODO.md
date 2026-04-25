# aichronicles TODO

Loose tracking for follow-ups not yet worth their own issue or branch.
Everything here is fair game; pick one off, sketch a plan, ship it.

## Open

### Add `LICENSE` (Apache-2.0)

The README references one but the file isn't committed yet. Drop in
the canonical Apache-2.0 text (e.g. via `gh repo create --license=
apache-2.0` or by copying from
https://www.apache.org/licenses/LICENSE-2.0.txt) before the first
public-release commit. Confirm the README's License section matches.

### Shell completion for `--session` (and the `summaries show` arg)

Today users type 8-char prefixes and `store.ResolveSessionIDPrefix`
expands them. The next step is shell completion: as soon as the user
starts typing a session id (or prefix), tab cycles through matching
sessions from the live store.

Scope:

- Add a `cobra.Command.ValidArgsFunction` (for `summaries show <session>`)
  and `cmd.RegisterFlagCompletionFunc("session", ...)` on the three
  subcommands that take `--session`: `summarize`, `search`, `summaries
  list`.
- Each completion func opens the store read-only, queries
  `SELECT id FROM sessions WHERE id LIKE ? || '%' ORDER BY ...`,
  and returns the matching ids (full uuid, not the 8-char preview —
  the shell handles partial matching from the user's current input).
  Cap to ~50 candidates so a blank tab doesn't list thousands.
- Surface the cwd and the first user prompt as the completion
  description (cobra's `cobra.ShellCompDirective` + tab-separated
  `id\tdescription`). Makes "which session was the chainguard one?"
  obvious without a second command.
- Wire `aichronicles completion <bash|zsh|fish>` (cobra has it built-in
  via `cmd.GenFishCompletion` etc.) — or rely on cobra's default
  `completion` subcommand which Execute() auto-installs.
- Document the install path in `docs/getting-started.md` once docs land.

Implementation hint: opening the store inside a completion func is
fine — it's read-only, the daemon's WAL handles concurrency, and tab
completion is interactive (a 50ms query is invisible).
