## aichronicles setup systemd

Install socket-activated systemd --user units

### Synopsis

Writes the api and web-UI unit pairs into
~/.config/systemd/user/, reloads the user manager, and enables
both sockets so the matching service starts on demand when
someone connects:

  - aichronicles-api.socket    UDS for the api daemon
  - aichronicles-api.service   long-lived JSON/SSE daemon; sole SQLite writer
  - aichronicles-web.socket    TCP 127.0.0.1:7878 for the web UI
  - aichronicles-web.service   web UI; idle-shutdown after 5m

The service units expect `aichronicles` and `aichronicles-api`
to be discoverable on systemd's user manager PATH (~/.local/bin
by default, via `make install`).

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
