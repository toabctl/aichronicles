## aichronicles setup cron

Install systemd --user timers for aichronicles' canonical scheduled tasks

### Synopsis

Writes a fixed, opinionated set of systemd --user units into
~/.config/systemd/user/, reloads the user manager, and enables
each timer.

Today this is one timer: a weekly digest (`aichronicles digest
weekly`) that runs every Monday 09:00 UTC. Future canonical
schedules (e.g. nightly prune) slot in here without changing
the install command.

List installed timers with `systemctl --user list-timers`.
Trigger an immediate run with `systemctl --user start
<unit>.service`. Remove with `aichronicles teardown cron`.

Idempotent. Requires `systemctl` on PATH.

```
aichronicles setup cron [flags]
```

### Options

```
  -h, --help              help for cron
      --unit-dir string   systemd user-unit directory (default: ~/.config/systemd/user)
```

### SEE ALSO

* [aichronicles setup](./aichronicles_setup.md)	 - Install aichronicles into an AI coding agent or the OS
