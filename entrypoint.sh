#!/bin/sh
# Entry-point for the plex-watchparty container.
#
# Runs briefly as root so we can guarantee WORK_DIR is owned by the
# app user (UID 10001) before handing the long-lived process off to
# that user. Without this, a fresh host bind-mount (./data on the
# docker compose host) is root-owned and the app fails immediately
# with `mkdir: permission denied`.
set -e

WORK_DIR="${WORK_DIR:-/tmp/plexwatchparty}"
mkdir -p "$WORK_DIR"
chown -R app:app "$WORK_DIR"

# su-exec is the alpine-friendly equivalent of gosu: drops to the
# named user and exec()'s the command in-place (no extra process).
exec su-exec app /usr/local/bin/plexwatchparty
