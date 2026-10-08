#!/usr/bin/env bash
# Build double-click Windows installer packages for the agent.
#
# Produces, under dist/windows/, one folder and one .zip per architecture:
#   BackupAgent-Installer-x64/{backup-agent.exe,Install.cmd,Uninstall.cmd,README.txt}
#   BackupAgent-Installer-x86/...
# Copy a zip to a client, extract it, and double-click Install.cmd — no
# command line needed. A server-downloaded installer embeds enrollment; these
# generic packages prompt for the server/token/fingerprint on first run.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X centralbackup/internal/agent.Version=$VERSION"
OUT="dist/windows"
TPL="deploy/windows-installer"
rm -rf "$OUT"
mkdir -p "$OUT"

# to_crlf copies a text file converting LF -> CRLF so Windows shows it tidily.
to_crlf() { sed 's/$/\r/' "$1" > "$2"; }

build_one() {
    local goarch="$1" label="$2"
    local dir="$OUT/BackupAgent-Installer-$label"
    mkdir -p "$dir"
    echo "Building $label agent ($VERSION)..."
    CGO_ENABLED=0 GOOS=windows GOARCH="$goarch" \
        go build -ldflags "$LDFLAGS" -o "$dir/backup-agent.exe" ./cmd/agent
    for f in Install.cmd Uninstall.cmd README.txt; do
        to_crlf "$TPL/$f" "$dir/$f"
    done
    ( cd "$OUT" && zip -qr "BackupAgent-Installer-$label.zip" "BackupAgent-Installer-$label" )
    echo "  -> $dir  (+ .zip)"
}

build_one amd64 x64
build_one 386   x86

echo "Done:"
ls -lh "$OUT"/*.zip
