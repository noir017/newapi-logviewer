# Log retention

Read [Storage](../README.md#storage) first: with the folding archive in place,
New API's `*.log` directory is a **spool**, not a record. The permanent copy is
`ARCHIVE_DIR`, and it is ~10x smaller than the raw log it came from.

That changes what rotation is for.

## You probably do not need logrotate

If `LOG_DIR` is on tmpfs and the viewer is running, the spool manages itself:
each finished call is folded into the archive and its spool file is deleted once
consumed (`SPOOL_KEEP_MIN`, `SPOOL_MAX_MB`). Nothing accumulates.

Check it is working:

```bash
docker exec <container> df -h /app/logs      # should stay near-empty
curl -s localhost:7071/logviewer/healthz     # archive_ok, last_append_age_sec, spool_bytes
```

Read `archive_ok` and `last_append_age_sec`, not `pending`. A stalled archive
leaves `pending` at 0 — the stall deadline drains it whether writes are landing
or failing — so `pending` alone was green through both real outages. The
endpoint returns 503 when ingest is actually broken.

A spool that keeps growing means ingest has stopped. The usual cause is
permissions: the viewer runs as `nobody` and needs to unlink files from the
spool directory, which is a permission on the *directory*, not the files. The
combined image's entrypoint chowns it for exactly this reason.

## If you keep the raw logs on disk

Some people want the untouched log as well — the archive is lossy by design
(chunk envelopes are discarded), so byte-level fidelity means keeping the
original.

Compress it; do not delete it. The raw log compresses ~41x with gzip and ~73x
with xz, because it is overwhelmingly repeated envelope:

```bash
# hourly: compress every log New API is no longer writing to
0 * * * * cd /path/to/logs && \
  newest=$(ls -t *.log 2>/dev/null | head -1); \
  for f in *.log; do \
    [ "$f" = "$newest" ] && continue; \
    gzip -6 "$f"; \
  done
```

Skipping the newest file matters: New API holds it open and does not reopen on
`SIGHUP`, so compressing (or renaming) it under the process leaves it writing to
a deleted inode and the log silently stops.

For a `logrotate` rule, the same constraint applies — and note there is no
`rotate N`, deliberately:

```
/path/to/new-api/logs/*.log {
    daily
    maxsize 200M
    missingok
    notifempty
    compress
    delaycompress
    copytruncate
    # no `rotate`/`maxage`: nothing here should ever be deleted
}
```

`copytruncate` for the open-file-handle reason above.

Compressed files are invisible to the viewer, which globs `*.log`. That is fine:
the records are already in the archive. If you ever need one back, decompress it
into a scratch directory and point a second viewer at it, or use `-reingest` to
graft its records onto the live archive.

## Disabling body logging

`DEBUG=true` is what makes this viewer useful. If you only want it occasionally,
leave it off and flip it on when debugging — nothing is captured retroactively.

## Sensitivity

The content is every prompt, every response, every tool argument, in plaintext.
Both the spool and the archive deserve the same care as a database backup, and
`AUTH_MODE=none` means anyone who reaches the URL reads all of it.
