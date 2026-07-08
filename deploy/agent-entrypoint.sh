#!/bin/sh
# Containerized agent entrypoint: enroll on first start (using CB_SERVER,
# CB_TOKEN and optionally CB_FINGERPRINT / CB_NAME), then run the daemon.
set -e

STATE_DIR="${CB_STATE_DIR:-/var/lib/backup-agent}"

if [ ! -f "$STATE_DIR/creds.json" ]; then
    if [ -z "$CB_SERVER" ] || [ -z "$CB_TOKEN" ]; then
        echo "Not enrolled and CB_SERVER/CB_TOKEN not set." >&2
        echo "Set CB_SERVER, CB_TOKEN (and ideally CB_FINGERPRINT) on first run." >&2
        exit 1
    fi
    set -- --server "$CB_SERVER" --token "$CB_TOKEN"
    [ -n "$CB_FINGERPRINT" ] && set -- "$@" --fingerprint "$CB_FINGERPRINT"
    [ -n "$CB_NAME" ] && set -- "$@" --name "$CB_NAME"
    backup-agent enroll "$@" --state-dir "$STATE_DIR"
fi

exec backup-agent run --state-dir "$STATE_DIR"
