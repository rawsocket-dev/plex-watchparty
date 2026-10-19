#!/bin/sh
# Entry-point for the plex-watchparty container.
#
# Runs briefly as root so we can guarantee the app's data directories are
# owned by the app user (UID 10001) before handing the long-lived process
# off to that user. Without this, a fresh host bind-mount (./data on the
# docker compose host) is root-owned and the app fails immediately with
# `mkdir: permission denied`.
set -e

WORK_DIR="${WORK_DIR:-/tmp/plexwatchparty}"
# The app keeps its segment cache, poster art and small JSON state files
# one level ABOVE WORK_DIR (filepath.Dir(WORK_DIR) in main.go), so that
# parent has to be writable by the app user as well.
DATA_DIR="$(dirname "$WORK_DIR")"
mkdir -p "$WORK_DIR" "$DATA_DIR/cache" "$DATA_DIR/posters"
chown -R app:app "$WORK_DIR" "$DATA_DIR/cache" "$DATA_DIR/posters"
# Own the data root itself (not recursively — it may be a shared parent)
# plus the state files the app writes there, so files restored or copied
# in as root stay writable. Shared system directories are left alone.
case "$DATA_DIR" in
  / | /tmp | /var/tmp) ;;
  *)
    chown app:app "$DATA_DIR"
    for f in library-cache.json recent.json state.json host.json audit.jsonl aliases.json; do
      if [ -e "$DATA_DIR/$f" ]; then chown app:app "$DATA_DIR/$f"; fi
    done
    ;;
esac

# su-exec is the alpine-friendly equivalent of gosu: drops to the
# named user and exec()'s the command in-place (no extra process).
exec su-exec app /usr/local/bin/plexwatchparty
