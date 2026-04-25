## aichronicles setup systemd

Install socket-activated systemd --user units

### Synopsis

Writes aichronicles.socket and aichronicles.service into
~/.config/systemd/user/, reloads the user manager, and enables
the socket so aichroniclesd starts on demand when a hook
connects. The service unit expects `aichroniclesd` to be
discoverable on systemd's user manager PATH.

Requires `systemctl` on PATH. Idempotent.

```
aichronicles setup systemd [flags]
```

### Options

```
  -h, --help              help for systemd
      --unit-dir string   systemd user-unit directory (default: ~/.config/systemd/user)
```

### SEE ALSO

* [aichronicles setup](./aichronicles_setup.md)	 - Install aichronicles into an AI coding agent or the OS
