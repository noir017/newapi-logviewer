#!/usr/bin/env bash
# Runs new-api and the log viewer in one container.
#
# Two processes in one container needs three things done right, and getting any
# of them wrong is worse than running two containers:
#
#   1. Signal forwarding. Docker sends SIGTERM to PID 1 only. Without an
#      explicit trap, new-api never gets it and every `docker stop` becomes a
#      10s SIGKILL - dropping database connections mid-write.
#   2. Fail-fast. If either process dies the container must exit, so the
#      restart policy applies. A plain `a & b & wait` keeps the container
#      "up" with half of it dead, and the healthcheck is the only hint.
#   3. Least privilege. new-api needs root (it writes /data); the viewer needs
#      to read the spool and delete files from it once they are archived, so it
#      owns the spool directory rather than running as root.
set -uo pipefail

: "${LOGVIEWER_ENABLED:=true}"
: "${LOG_DIR:=/app/logs}"
: "${ARCHIVE_DIR:=/app/archive}"
: "${LOGVIEWER_PORT:=7070}"
: "${BASE_PATH:=/logviewer}"
: "${VIEWER_UID:=65534}"
: "${VIEWER_GID:=65534}"

newapi_pid=""
viewer_pid=""
spool_chown_pid=""
shutting_down=0

log() { echo "[entrypoint] $*"; }

terminate() {
  # Guard against re-entry: the EXIT trap fires after the signal trap.
  [ "$shutting_down" = 1 ] && return
  shutting_down=1
  log "shutting down"
  # The spool watcher first: it is an infinite sleep loop, so leaving it alive
  # would make the `wait` calls below hang until the container is SIGKILLed.
  if [ -n "$spool_chown_pid" ] && kill -0 "$spool_chown_pid" 2>/dev/null; then
    kill -TERM "$spool_chown_pid" 2>/dev/null || true
  fi
  # new-api next, and give it time to close DB connections before the
  # container's own grace period runs out.
  if [ -n "$newapi_pid" ] && kill -0 "$newapi_pid" 2>/dev/null; then
    kill -TERM "$newapi_pid" 2>/dev/null || true
  fi
  if [ -n "$viewer_pid" ] && kill -0 "$viewer_pid" 2>/dev/null; then
    kill -TERM "$viewer_pid" 2>/dev/null || true
  fi
  wait "$newapi_pid" 2>/dev/null || true
  wait "$viewer_pid" 2>/dev/null || true
}
trap terminate TERM INT

# LOG_DIR is a spool, not the record of what happened. new-api writes every
# streaming chunk there - 76% of its log volume, ~98% of it a repeated envelope
# around a few characters - and the viewer folds finished calls into ARCHIVE_DIR
# and deletes the spool file. Mount LOG_DIR on tmpfs so that traffic never
# reaches a disk; ARCHIVE_DIR is the part that must be persistent.
mkdir -p "$LOG_DIR" "$ARCHIVE_DIR"

if [ "$LOGVIEWER_ENABLED" = "true" ]; then
  # The viewer deletes spool files it has archived, so it needs write access to
  # both directories. Granting ownership is narrower than running it as root:
  # it still cannot touch /data or anything else new-api owns.
  #
  # new-api runs as root and creates its log file 0644. A non-owner can read it
  # but not unlink it - unlinking is a DIRECTORY permission, so owning the
  # directory is what matters for pruning, and the files can stay root's.
  #
  # Truncating is different: it is a permission on the FILE. The viewer truncates
  # the live spool file to reclaim tmpfs once its bytes are archived, which is
  # the normal case here because new-api never rotates - one file per process
  # start, growing forever. With a root-owned 0644 file that fails with EPERM,
  # and the spool climbed to 93% of its tmpfs while the viewer logged
  # "permission denied" on every sweep.
  #
  # Two things that look like fixes and are not, both ruled out on the running
  # container rather than by reasoning:
  #
  #   - a umask on new-api. It passes 0644 explicitly, and a umask can only
  #     clear bits. /proc/<pid>/status showed Umask 0111 and the file was 0644.
  #   - the viewer chmod-ing the file itself. chmod requires OWNING the file;
  #     owning the directory is not enough. EPERM as uid 65534.
  #
  # What does work is chown-ing the file, which needs root - so a small watcher
  # does it. new-api names the log after its own start time, so the name cannot
  # be pre-created; and it opens with O_APPEND and no O_CREAT, so it will not
  # recreate or replace a file that already exists. Handing it over is safe.
  chown "$VIEWER_UID:$VIEWER_GID" "$LOG_DIR" "$ARCHIVE_DIR" 2>/dev/null || \
    log "WARNING: could not chown $LOG_DIR/$ARCHIVE_DIR; spool pruning will fail"

  # Hand every spool file to the viewer's uid, now and as new ones appear.
  #
  # Polling, not inotify: this image has no inotify tools, the spool holds one or
  # two files, and a stat every few seconds costs nothing. It must keep running
  # because new-api opens a new log on every restart, and an unowned file is
  # exactly the failure this exists to prevent.
  #
  # Deliberately NOT tied to the fail-fast `wait -n` below: if this watcher dies
  # the spool stops being reclaimable, which is degraded but not worth killing a
  # working gateway for. /healthz reports it as spool_truncate_error.
  (
    while :; do
      for f in "$LOG_DIR"/*.log; do
        [ -e "$f" ] || continue
        # -c so an already-correct file is not touched every pass.
        chown -c "$VIEWER_UID:$VIEWER_GID" "$f" 2>/dev/null || true
      done
      sleep 5
    done
  ) &
  spool_chown_pid=$!
  log "spool owner watcher started (pid $spool_chown_pid)"

  # setpriv, not su: no PAM, no intermediate shell, so the viewer is a direct
  # child of this script and `wait -n` sees it exit.
  # --init-groups would need a valid supplementary group set; nobody has one
  # group and this is all the access it needs.
  LOG_DIR="$LOG_DIR" ARCHIVE_DIR="$ARCHIVE_DIR" PORT="$LOGVIEWER_PORT" BASE_PATH="$BASE_PATH" \
    setpriv --reuid="$VIEWER_UID" --regid="$VIEWER_GID" --clear-groups \
    /usr/local/bin/logviewer &
  viewer_pid=$!
  log "log viewer started (pid $viewer_pid, uid $VIEWER_UID) on :$LOGVIEWER_PORT$BASE_PATH"
  log "spool=$LOG_DIR archive=$ARCHIVE_DIR"
else
  log "log viewer disabled (LOGVIEWER_ENABLED=$LOGVIEWER_ENABLED)"
fi

# The stale comment that used to sit here claimed a umask made new-api's log
# group-writable. It does not: new-api passes 0644 explicitly and a umask can
# only clear bits. The spool watcher above is what makes the log reclaimable.
/new-api "$@" &
newapi_pid=$!
log "new-api started (pid $newapi_pid)"

# Return as soon as either REAL service exits, rather than waiting for both.
#
# Not `wait -n`: the spool watcher is also a child, and `wait -n` would return
# for it too - turning a degraded-but-harmless watcher crash into a container
# restart. Poll the two pids that matter instead, at a granularity that is
# irrelevant next to the restart itself.
rc=0
while :; do
  if [ -n "$newapi_pid" ] && ! kill -0 "$newapi_pid" 2>/dev/null; then
    wait "$newapi_pid"; rc=$?
    break
  fi
  if [ -n "$viewer_pid" ] && ! kill -0 "$viewer_pid" 2>/dev/null; then
    wait "$viewer_pid"; rc=$?
    break
  fi
  # The watcher is not fatal, but a silent disappearance would leave the spool
  # unreclaimable with nothing in the log to say why.
  if [ -n "$spool_chown_pid" ] && ! kill -0 "$spool_chown_pid" 2>/dev/null; then
    log "WARNING: spool owner watcher exited; the spool may stop being reclaimable"
    spool_chown_pid=""
  fi
  sleep 1
done

# Identify which one died, for a log line that actually says what happened.
if [ -n "$newapi_pid" ] && ! kill -0 "$newapi_pid" 2>/dev/null; then
  log "new-api exited (code $rc) - stopping container"
elif [ -n "$viewer_pid" ] && ! kill -0 "$viewer_pid" 2>/dev/null; then
  log "log viewer exited (code $rc) - stopping container"
else
  log "a child exited (code $rc) - stopping container"
fi

terminate
exit "$rc"
