# Multi-target build:
#   docker build -f deploy/Dockerfile --target server -t centralbackup/server .
#   docker build -f deploy/Dockerfile --target agent  -t centralbackup/agent .
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN set -e; \
    LDFLAGS="-s -w -X centralbackup/internal/agent.Version=${VERSION}"; \
    CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o /out/backup-server ./cmd/server; \
    CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o /out/oauth-forwarder ./cmd/oauth-forwarder; \
    CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags "$LDFLAGS" -o /out/agents/backup-agent-linux-amd64 ./cmd/agent; \
    CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -ldflags "$LDFLAGS" -o /out/agents/backup-agent-linux-arm64 ./cmd/agent; \
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAGS" -o /out/agents/backup-agent-windows-amd64.exe ./cmd/agent; \
    CGO_ENABLED=0 GOOS=windows GOARCH=386   go build -ldflags "$LDFLAGS" -o /out/agents/backup-agent-windows-386.exe ./cmd/agent

# Legacy 32-bit agent for Windows 7 SP1 / Embedded POSReady 7 (NT 6.1), which
# the standard Go 1.25 binaries cannot run on. It needs the Go 1.20 toolchain,
# fetched on demand via GOPROXY. Best-effort: git is optional (VERSION is
# passed in) and a failure here does not fail the image build — the installer
# endpoint simply 404s that platform if the binary is absent.
RUN apk add --no-cache git >/dev/null 2>&1 || true; \
    VERSION="${VERSION}" sh scripts/build-legacy-agent.sh /out/agents \
    || echo "WARNING: legacy Windows 7 agent not built into this image"

# ---- central server image ----
FROM alpine:3.21 AS server
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/backup-server /app/backup-server
# Prebuilt agent binaries, served at /dl/ for the client install scripts.
COPY --from=build /out/agents /app/agents
ENV CB_DATA_DIR=/data \
    CB_LISTEN=:8443 \
    CB_AGENT_BIN_DIR=/app/agents
VOLUME /data
EXPOSE 8443
ENTRYPOINT ["/app/backup-server"]

# ---- central storage-OAuth callback (multi-tenant deployments only) ----
# One redirect URI shared by every tenant; routes the provider's callback to
# the tenant named in the OAuth state. See docs/central-oauth-callback.md.
FROM alpine:3.21 AS oauth-forwarder
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/oauth-forwarder /app/oauth-forwarder
ENV CB_LISTEN=:8080
EXPOSE 8080
ENTRYPOINT ["/app/oauth-forwarder"]

# ---- containerized agent image (for Docker hosts) ----
FROM alpine:3.21 AS agent
RUN apk add --no-cache ca-certificates tzdata docker-cli
COPY --from=build /out/agents/backup-agent-linux-amd64 /usr/local/bin/backup-agent
COPY deploy/agent-entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
VOLUME /var/lib/backup-agent
ENTRYPOINT ["/entrypoint.sh"]
