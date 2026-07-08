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
    CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags "$LDFLAGS" -o /out/agents/backup-agent-linux-amd64 ./cmd/agent; \
    CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -ldflags "$LDFLAGS" -o /out/agents/backup-agent-linux-arm64 ./cmd/agent; \
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAGS" -o /out/agents/backup-agent-windows-amd64.exe ./cmd/agent

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

# ---- containerized agent image (for Docker hosts) ----
FROM alpine:3.21 AS agent
RUN apk add --no-cache ca-certificates tzdata docker-cli
COPY --from=build /out/agents/backup-agent-linux-amd64 /usr/local/bin/backup-agent
COPY deploy/agent-entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
VOLUME /var/lib/backup-agent
ENTRYPOINT ["/entrypoint.sh"]
