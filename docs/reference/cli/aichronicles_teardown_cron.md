## aichronicles teardown cron

Remove aichronicles systemd --user cron timers

### Synopsis

Inverse of `setup cron`. Disables + stops every aichronicles
cron timer, deletes the unit files from ~/.config/systemd/user/,
and reloads the user manager. Idempotent: running when nothing
is installed is a no-op.

Dry-run by default; pass --yes to actually remove.

```
aichronicles teardown cron [flags]
```

### Options

```
  -h, --help              help for cron
      --unit-dir string   systemd user-unit directory (default: ~/.config/systemd/user)
      --yes               confirm the removal (required to invoke systemctl and delete files)
```

### SEE ALSO

* [aichronicles teardown](./aichronicles_teardown.md)	 - Remove aichronicles integration from an AI coding agent or the OS
