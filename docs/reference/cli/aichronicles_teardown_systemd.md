## aichronicles teardown systemd

Remove aichronicles systemd --user units

### Synopsis

Disables + stops both unit pairs (aichronicles-api.socket /
.service for the daemon; aichronicles-web.socket / .service
for the web UI), deletes the unit files from
~/.config/systemd/user/, and reloads the user manager.
Also removes the legacy aichronicles.{socket,service} units
installed by older versions before the api rearchitecture.
Idempotent: running when nothing is installed is a no-op.

Runs in dry-run mode by default: it reports what would be
disabled and deleted without invoking systemctl or removing
files. Pass --yes to actually remove.

```
aichronicles teardown systemd [flags]
```

### Options

```
  -h, --help              help for systemd
      --unit-dir string   systemd user-unit directory (default: ~/.config/systemd/user)
      --yes               confirm the removal (required to invoke systemctl and delete files)
```

### SEE ALSO

* [aichronicles teardown](./aichronicles_teardown.md)	 - Remove aichronicles integration from an AI coding agent or the OS
