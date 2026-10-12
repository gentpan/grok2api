#!/bin/sh
# Container entrypoint: repair ownership of the state directories, then drop
# privileges to the unprivileged `app` user and exec the service.
#
# It starts as root only so a bind-mounted ./data (usually owned by another
# uid, or written by root in earlier images) can be handed to `app`. When the
# service could not write to a configured path as `app` (read-only or
# non-chownable mounts such as NFS, or ACCOUNT_LOCAL_PATH / CONFIG_PATH outside
# the data directory) it runs as root instead of failing at startup, and says
# so. Started with `docker run --user ...` it simply execs the command.
set -e

if [ "$(id -u)" != "0" ]; then
    exec "$@"
fi

data_dir="${DATA_DIR:-/app/data}"
log_dir="${LOG_DIR:-/app/logs}"

# fix_dir creates dir, hands everything in it to app and reports whether app
# can write there afterwards.
fix_dir() {
    mkdir -p "$1" 2>/dev/null || true
    find "$1" ! -user app -exec chown app:app {} + 2>/dev/null || true
    su-exec app:app test -w "$1"
}

# check_dir only reports writability: a configured file path may live in a
# directory we must not re-own (for example /etc), so it is never chowned.
check_dir() {
    su-exec app:app test -w "$1"
}

ok=1
fix_dir "$data_dir" || ok=0
fix_dir "$log_dir" || ok=0
if [ -n "${ACCOUNT_LOCAL_PATH:-}" ]; then
    check_dir "$(dirname "$ACCOUNT_LOCAL_PATH")" || ok=0
fi
if [ -n "${CONFIG_PATH:-}" ]; then
    check_dir "$(dirname "$CONFIG_PATH")" || ok=0
fi

if [ "$ok" = "1" ]; then
    exec su-exec app:app "$@"
fi

echo "WARNING: a data/log/config path is not writable by uid 10001 (read-only or non-chownable mount); running as root" >&2
exec "$@"
