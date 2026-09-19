#!/bin/sh
# Build the 32-bit Windows agent for Windows 7 SP1 / Windows Embedded
# POSReady 7 (NT 6.1) and other pre-Windows-10 systems.
#
# Why this exists: the Go 1.25 toolchain used for the normal builds only
# targets Windows 10 / Server 2016 and newer. A binary it produces crashes
# at startup on Windows 7 (an access violation in runtime.asmstdcall as the
# runtime calls a kernel API that does not exist on that OS). Go 1.20 is the
# last toolchain that still targets Windows 7 SP1, and it stamps the PE with
# MinOSVersion 6.1 so the loader accepts it.
#
# The agent code is kept buildable on Go 1.20 (no >=1.21 language/stdlib
# features — see the note in internal/agent/engine.go). Only two dependencies
# need pinning back to their last 1.20-compatible release; that is done here
# in a throwaway copy of the module so the committed go.mod stays on 1.25 for
# the server, which needs newer Go.
#
# Usage: scripts/build-legacy-agent.sh [OUTPUT_DIR]   (default: dist/agents)
set -eu

GO_TOOLCHAIN="${LEGACY_GO_TOOLCHAIN:-go1.20.14}"
# Last releases of these that compile under Go 1.20.
COMPRESS_VER="v1.17.11"
XSYS_VER="v0.15.0"

repo_root=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
version="${VERSION:-$(git -C "$repo_root" describe --tags --always 2>/dev/null || echo dev)}"

# Resolve the output dir to an absolute path *before* we cd into the temp
# build tree below — otherwise a relative path (e.g. "dist/agents") would
# land inside the throwaway copy and be deleted with it.
out_dir="${1:-$repo_root/dist/agents}"
mkdir -p "$out_dir"
out_dir=$(CDPATH= cd "$out_dir" && pwd)

build_dir=$(mktemp -d)
trap 'rm -rf "$build_dir"' EXIT

echo "Legacy 32-bit Windows agent (Windows 7 / POSReady 7)"
echo "  toolchain: $GO_TOOLCHAIN   version: $version"

# Throwaway copy of just what the agent build needs.
cp -r "$repo_root/cmd" "$repo_root/internal" "$repo_root/go.mod" "$repo_root/go.sum" "$build_dir"/
cd "$build_dir"

# Lower the module's Go requirement and pin the two deps whose current
# releases require a newer toolchain than Windows 7 support allows. A
# temp-file rewrite keeps this portable across GNU/BSD/busybox sed (the
# Docker builder is busybox, dev machines may be macOS).
sed 's/^go 1\.[0-9][0-9]*\(\.[0-9]*\)\{0,1\}.*/go 1.20/' go.mod > go.mod.tmp && mv go.mod.tmp go.mod
GOTOOLCHAIN="$GO_TOOLCHAIN" GOFLAGS=-mod=mod go get \
	"github.com/klauspost/compress@$COMPRESS_VER" \
	"golang.org/x/sys@$XSYS_VER" >/dev/null 2>&1

out="$out_dir/backup-agent-windows-386-legacy.exe"
GOTOOLCHAIN="$GO_TOOLCHAIN" CGO_ENABLED=0 GOOS=windows GOARCH=386 \
	go build -ldflags "-s -w -X centralbackup/internal/agent.Version=$version" \
	-o "$out" ./cmd/agent

echo "Built: $out"
