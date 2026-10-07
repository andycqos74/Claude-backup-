#!/usr/bin/env bash
# Cross-compile release binaries into dist/.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X centralbackup/internal/agent.Version=$VERSION"
mkdir -p dist/agents

echo "Building version $VERSION"
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags "$LDFLAGS" -o dist/backup-server ./cmd/server
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags "$LDFLAGS" -o dist/agents/backup-agent-linux-amd64 ./cmd/agent
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -ldflags "$LDFLAGS" -o dist/agents/backup-agent-linux-arm64 ./cmd/agent
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAGS" -o dist/agents/backup-agent-windows-amd64.exe ./cmd/agent
# 32-bit Windows, for the remaining x86-only machines. See docs/deployment.md.
CGO_ENABLED=0 GOOS=windows GOARCH=386   go build -ldflags "$LDFLAGS" -o dist/agents/backup-agent-windows-386.exe ./cmd/agent

# Legacy 32-bit build for Windows 7 SP1 / POSReady 7. It is built out-of-band
# with the Go 1.20 toolchain and committed under deploy/prebuilt (regenerate
# with scripts/build-legacy-agent.sh when the agent changes); here we just
# ship that committed copy so a release never depends on a 1.20 toolchain.
if [ -f deploy/prebuilt/backup-agent-windows-386-legacy.exe ]; then
    cp deploy/prebuilt/backup-agent-windows-386-legacy.exe dist/agents/
else
    echo "WARNING: deploy/prebuilt/backup-agent-windows-386-legacy.exe missing; run scripts/build-legacy-agent.sh deploy/prebuilt" >&2
fi

echo "Done:"
ls -lh dist dist/agents
