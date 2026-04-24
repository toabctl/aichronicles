# systemd --user units for aichronicles

Socket-activated user service. systemd owns the Unix socket; the daemon
is spawned on first connect (a Claude hook POSTing an envelope) and
supervised by systemd thereafter.

## Install

Copy or symlink both unit files to `~/.config/systemd/user/`:

```bash
install -Dm644 aichronicles.socket  ~/.config/systemd/user/aichronicles.socket
install -Dm644 aichronicles.service ~/.config/systemd/user/aichronicles.service
systemctl --user daemon-reload
systemctl --user enable --now aichronicles.socket
```

If you manage dotfiles with GNU Stow, a `systemd` package that mirrors
`~/.config/systemd/user/` is the natural place for these.

## Verify

```bash
# socket is active, listening at $XDG_RUNTIME_DIR/aichronicles/sock
systemctl --user status aichronicles.socket

# logs from the daemon itself
journalctl --user -u aichronicles.service -f
```

Fire a test envelope:

```bash
curl -sS --unix-socket "$XDG_RUNTIME_DIR/aichronicles/sock" http://x/v1/healthz
```

systemd should spawn the daemon on demand.

## What the units do

- `aichronicles.socket` — listens at `%t/aichronicles/sock` with `0600` perms
  (`%t` expands to `$XDG_RUNTIME_DIR`). `RemoveOnStop=true` cleans up the
  socket file when the unit stops.
- `aichronicles.service` — `ExecStart=aichroniclesd`, restart-on-failure,
  depends on the socket unit. The daemon detects `LISTEN_FDS` at
  startup and uses systemd's fd instead of opening its own listener.

## Uninstall

```bash
systemctl --user disable --now aichronicles.socket aichronicles.service
rm ~/.config/systemd/user/aichronicles.{socket,service}
systemctl --user daemon-reload
```
