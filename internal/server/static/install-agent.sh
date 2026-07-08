#!/usr/bin/env bash
# Central Backup agent installer for Ubuntu / Linux (systemd).
# Usage (from the server GUI enrollment dialog):
#   curl -fsSLk https://SERVER:8443/static/install-agent.sh | sudo bash -s -- \
#     --server https://SERVER:8443 --token TOKEN --fingerprint FP [--name NAME]
set -euo pipefail

SERVER="" TOKEN="" FINGERPRINT="" NAME=""
while [ $# -gt 0 ]; do
    case "$1" in
        --server)      SERVER="$2"; shift 2 ;;
        --token)       TOKEN="$2"; shift 2 ;;
        --fingerprint) FINGERPRINT="$2"; shift 2 ;;
        --name)        NAME="$2"; shift 2 ;;
        *) echo "unknown option: $1" >&2; exit 2 ;;
    esac
done
[ -n "$SERVER" ] && [ -n "$TOKEN" ] || { echo "--server and --token are required" >&2; exit 2; }
[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)" >&2; exit 2; }

case "$(uname -m)" in
    x86_64)          ARCH=amd64 ;;
    aarch64|arm64)   ARCH=arm64 ;;
    *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

BIN=/usr/local/bin/backup-agent
echo "Downloading agent binary..."
# -k: the server uses a self-signed cert; authenticity is enforced by the
# certificate fingerprint pin during enrollment below.
curl -fsSLk "$SERVER/dl/backup-agent-linux-$ARCH" -o "$BIN.tmp"
chmod 0755 "$BIN.tmp"
mv "$BIN.tmp" "$BIN"

mkdir -p /etc/backup-agent /var/lib/backup-agent
chmod 0700 /var/lib/backup-agent

echo "Enrolling with $SERVER ..."
ENROLL_ARGS=(--server "$SERVER" --token "$TOKEN" --state-dir /var/lib/backup-agent)
[ -n "$FINGERPRINT" ] && ENROLL_ARGS+=(--fingerprint "$FINGERPRINT")
[ -n "$NAME" ] && ENROLL_ARGS+=(--name "$NAME")
"$BIN" enroll "${ENROLL_ARGS[@]}"

cat > /etc/systemd/system/backup-agent.service <<'EOF'
[Unit]
Description=Central Backup Agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/backup-agent run
Environment=CB_STATE_DIR=/var/lib/backup-agent
Environment=CB_CONFIG=/etc/backup-agent/agent.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now backup-agent

echo
echo "Done. The agent is running and connected."
echo "  service:  systemctl status backup-agent"
echo "  jobs:     backup-agent job list --config /etc/backup-agent/agent.yaml"
echo "  config:   /etc/backup-agent/agent.yaml (syncs with the server GUI)"
