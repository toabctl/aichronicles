## aichronicles doctor

Probe the running aichronicles-api daemon and report health

### Synopsis

doctor performs a real connect + roundtrip against the
daemon's UDS healthz endpoint and reports whether the api
is currently answering. Exits 0 when the daemon answers,
non-zero otherwise, so the command can be wired to a
status bar, a cron job, or a shell prompt indicator.

Catches the failure mode where the daemon process is
running and the kernel reports the socket as LISTEN but
connect() actually returns ECONNREFUSED — which silently
drops every hook fire and is invisible to `pgrep` or
`systemctl status`.

```
aichronicles doctor [flags]
```

### Options

```
  -h, --help            help for doctor
  -q, --quiet           suppress all output; use the exit code only
      --socket string   aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)
```

### SEE ALSO

* [aichronicles](./aichronicles.md)	 - Capture AI coding agent session events
