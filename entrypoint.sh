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
#   3. Least privilege. new-api needs root (it writes /data and the log dir);
#      the viewer only ever reads, so it drops to nobody.
set -uo pipefail

: "${LOGVIEWER_ENABLED:=true}"
: "${LOG_DIR:=/app/logs}"
: "${LOGVIEWER_PORT:=7070}"
: "${BASE_PATH:=/logviewer}"

newapi_pid=""
viewer_pid=""
shutting_down=0

log() { echo "[entrypoint] $*"; }

terminate() {
  # Guard against re-entry: the EXIT trap fires after the signal trap.
  [ "$shutting_down" = 1 ] && return
  shutting_down=1
  log "shutting down"
  # new-api first, and give it time to close DB connections before the
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

# The viewer reads the log directory, so it has to exist before it starts;
# new-api would otherwise create it on first write.
mkdir -p "$LOG_DIR"

if [ "$LOGVIEWER_ENABLED" = "true" ]; then
  # setpriv, not su: no PAM, no intermediate shell, so the viewer is a direct
  # child of this script and `wait -n` sees it exit.
  # --init-groups would need a valid supplementary group set; nobody has one
  # group and read-only access is all it needs.
  LOG_DIR="$LOG_DIR" PORT="$LOGVIEWER_PORT" BASE_PATH="$BASE_PATH" \
    setpriv --reuid=65534 --regid=65534 --clear-groups \
    /usr/local/bin/logviewer &
  viewer_pid=$!
  log "log viewer started (pid $viewer_pid, uid 65534) on :$LOGVIEWER_PORT$BASE_PATH"
else
  log "log viewer disabled (LOGVIEWER_ENABLED=$LOGVIEWER_ENABLED)"
fi

/new-api "$@" &
newapi_pid=$!
log "new-api started (pid $newapi_pid)"

# Return as soon as EITHER exits, rather than waiting for both.
wait -n
rc=$?

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
