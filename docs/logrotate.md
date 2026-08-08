# Rotating New API's debug logs

`DEBUG=true` is what makes this viewer useful, and also what makes New API's log
directory grow without bound. New API does not rotate these files itself.

The content is not incidental: every prompt, every response, every tool argument,
in plaintext. Treat the directory as sensitive and cap its size.

## logrotate

```
# /etc/logrotate.d/new-api
/path/to/new-api/logs/*.log {
    daily
    rotate 7
    maxsize 200M
    missingok
    notifempty
    compress
    delaycompress
    copytruncate
}
```

`copytruncate` matters: New API holds the file open and does not reopen on
`SIGHUP`, so a plain `create` rotation leaves it writing to a deleted inode and
the log silently stops.

Test the rule before trusting it:

```bash
logrotate -d /etc/logrotate.d/new-api    # dry run, prints what it would do
logrotate -f /etc/logrotate.d/new-api    # force one rotation
```

## Interaction with the viewer

The viewer globs `*.log`, so it stops seeing rotated files once they are renamed
to `.log.1` or compressed. That is usually what you want — but it means
**rotation is also retention**: whatever rotates out disappears from the UI.

Size the rotation to the window you actually want to browse.

`LIMIT_MB` (default 40) separately caps how much of *each* file is parsed, from
the end. If a single log exceeds it, older entries in that file are invisible
even before rotation. Raise it if you rotate infrequently:

```yaml
environment:
  - LIMIT_MB=200
```

Parsing is linear in bytes read, so this trades startup and refresh latency for
history.

## Hosts without logrotate

A cron job is enough:

```bash
# keep the last 3 files, rotate when the active log passes 200MB
0 * * * * cd /path/to/new-api/logs && \
  for f in *.log; do \
    [ $(stat -c%s "$f") -gt 209715200 ] && cp "$f" "$f.$(date +%s).old" && : > "$f"; \
  done; \
  ls -t *.old 2>/dev/null | tail -n +4 | xargs -r rm
```

`: > "$f"` truncates in place rather than deleting, for the same open-file-handle
reason as `copytruncate`.

## Disabling body logging

If you only need this occasionally, leave `DEBUG=false` normally and flip it on
when debugging:

```bash
docker compose exec new-api sh -c 'echo restart with DEBUG=true'
# edit compose, then:
docker compose up -d new-api
```

Nothing is logged retroactively — you only capture calls made while it was on.
