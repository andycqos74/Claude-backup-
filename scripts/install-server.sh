#!/usr/bin/env bash
# Docker-free server install for Ubuntu/Linux: builds the server binary from
# this repo and installs it as a systemd service. Run from the repo root:
#   sudo CB_SERVER_NAME=backup.example.com scripts/install-server.sh
#
# Requires Go 1.25+ to build (only at install time). Prefer the Docker path
# (deploy/docker-compose.yml) if you'd rather not install a toolchain.
set -euo pipefail
cd "$(dirname "$0")/.."

[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)" >&2; exit 2; }
command -v go >/dev/null || { echo "Go toolchain not found; install Go 1.25+ or use the Docker deployment" >&2; exit 1; }

DATA_DIR="${CB_DATA_DIR:-/var/lib/backup-server}"
LISTEN="${CB_LISTEN:-:8443}"
SERVER_NAME="${CB_SERVER_NAME:-}"

echo "Building server binary..."
CGO_ENABLED=0 go build -ldflags "-s -w -X centralbackup/internal/agent.Version=$(git describe --tags --always 2>/dev/null || echo dev)" \
    -o /usr/local/bin/backup-server ./cmd/server

echo "Building agent binaries (served to clients at /dl)..."
mkdir -p "$DATA_DIR/agents"
for target in linux/amd64 linux/arm64 windows/amd64; do
    os="${target%/*}"; arch="${target#*/}"; ext=""; [ "$os" = windows ] && ext=".exe"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -ldflags "-s -w" \
        -o "$DATA_DIR/agents/backup-agent-$os-$arch$ext" ./cmd/agent
done

install -d -m 0700 "$DATA_DIR"

cat > /etc/systemd/system/backup-server.service <<EOF
[Unit]
Description=Central Backup Server
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/backup-server
Environment=CB_DATA_DIR=$DATA_DIR
Environment=CB_LISTEN=$LISTEN
Environment=CB_AGENT_BIN_DIR=$DATA_DIR/agents
Environment=CB_SERVER_NAME=$SERVER_NAME
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now backup-server

echo
echo "Server installed and running on $LISTEN."
echo "  status:      systemctl status backup-server"
echo "  logs:        journalctl -u backup-server -f"
echo "  fingerprint: journalctl -u backup-server | grep -i fingerprint | tail -1"
echo
echo "Open https://${SERVER_NAME:-<this-host>}${LISTEN} and create the admin account."
