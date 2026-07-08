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

echo "Done:"
ls -lh dist dist/agents
