#!/usr/bin/env bash
# End-to-end test for the multi-tenant agent plane (docs/multi-tenant-design.md,
# phase 1): two real single-tenant servers behind one nginx SNI router.
#
# Proves the load-bearing claim — that routing agents by SNI preserves
# certificate pinning and tenant isolation:
#
#   1. an agent enrolled for tenant A, connecting THROUGH the router, is
#      handed tenant A's certificate and its pin succeeds;
#   2. a real backup completes over the routed connection;
#   3. tenant B never sees tenant A's agent, job or snapshot;
#   4. tenant A's credentials pointed at tenant B's hostname fail the pin
#      (the router cannot be used to cross tenants);
#   5. connecting by IP literal sends no SNI and is refused, rather than
#      silently landing in some default tenant.
#
# Requires: go, nginx with ngx_stream_ssl_preread_module, jq, curl.
# Uses *.localtest.me (public DNS, resolves to 127.0.0.1), so no /etc/hosts
# edit and no root needed.
set -euo pipefail
cd "$(dirname "$0")/.."

ACME_HOST=acme.backup.localtest.me
GLOBEX_HOST=globex.backup.localtest.me
ROUTER_PORT=18443          # the only port agents ever use
ACME_PORT=18501            # tenant backends, never reached directly by agents
GLOBEX_PORT=18502
ACME_GUI=18601             # GUI listeners (would be behind cloudflared)
GLOBEX_GUI=18602

WORK="$(mktemp -d)"
PASS_COUNT=0

cleanup() {
    [ -n "${NGINX_STARTED:-}" ] && nginx -c "$WORK/nginx.conf" -s quit 2>/dev/null || true
    kill "${AGENT_PID:-0}" "${ACME_PID:-0}" "${GLOBEX_PID:-0}" 2>/dev/null || true
    wait 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

say()  { echo -e "\n=== $*"; }
pass() { PASS_COUNT=$((PASS_COUNT+1)); echo "  ok: $*"; }
fail() { echo "  FAIL: $*" >&2; exit 1; }

wait_for() { # wait_for SECONDS DESCRIPTION COMMAND...
    local deadline=$(( $(date +%s) + $1 )); shift
    local desc="$1"; shift
    while ! "$@" >/dev/null 2>&1; do
        [ "$(date +%s)" -lt "$deadline" ] || fail "timeout waiting for $desc"
        sleep 0.3
    done
}

# api TENANT METHOD PATH [BODY] — admin API against a tenant's GUI listener.
api() {
    local port="$1" method="$2" path="$3" body="${4:-}"
    local jar="$WORK/cookies-$port.txt"
    if [ -n "$body" ]; then
        curl -ksS -b "$jar" -c "$jar" -X "$method" -H 'X-Requested-With: fetch' \
            -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$port$path"
    else
        curl -ksS -b "$jar" -c "$jar" -X "$method" -H 'X-Requested-With: fetch' \
            "http://127.0.0.1:$port$path"
    fi
}

command -v nginx >/dev/null || fail "nginx not installed (needs ngx_stream_ssl_preread_module)"
nginx -V 2>&1 | grep -q stream_ssl_preread || fail "this nginx lacks ngx_stream_ssl_preread_module"
command -v jq >/dev/null || fail "jq not installed"

say "Building server + agent"
go build -o "$WORK/backup-server" ./cmd/server
go build -o "$WORK/backup-agent" ./cmd/agent

say "Starting two single-tenant servers"
start_tenant() { # start_tenant NAME HOST PORT GUI_PORT
    local name="$1" host="$2" port="$3" gui="$4"
    mkdir -p "$WORK/$name"
    CB_DATA_DIR="$WORK/$name/data" \
    CB_LISTEN="127.0.0.1:$port" \
    CB_GUI_LISTEN="127.0.0.1:$gui" \
    CB_SERVER_NAME="$host" \
    CB_PUBLIC_URL="https://$host:$ROUTER_PORT" \
    CB_GUI_URL="http://localhost:$gui" \
    CB_AGENT_BIN_DIR="$WORK/agents" \
        "$WORK/backup-server" >"$WORK/$name/server.log" 2>&1 &
    echo $!
}
ACME_PID=$(start_tenant acme "$ACME_HOST" "$ACME_PORT" "$ACME_GUI")
GLOBEX_PID=$(start_tenant globex "$GLOBEX_HOST" "$GLOBEX_PORT" "$GLOBEX_GUI")

wait_for 20 "acme server"   curl -ksS -o /dev/null "http://127.0.0.1:$ACME_GUI/login"
wait_for 20 "globex server" curl -ksS -o /dev/null "http://127.0.0.1:$GLOBEX_GUI/login"

ACME_FP=$(grep -oE '[0-9a-f]{64}' "$WORK/acme/server.log" | head -1)
GLOBEX_FP=$(grep -oE '[0-9a-f]{64}' "$WORK/globex/server.log" | head -1)
[ -n "$ACME_FP" ] && [ -n "$GLOBEX_FP" ] || fail "could not read tenant fingerprints"
[ "$ACME_FP" != "$GLOBEX_FP" ] || fail "tenants share a certificate — they must not"
pass "two tenants, two distinct certificates"
echo "     acme:   $ACME_FP"
echo "     globex: $GLOBEX_FP"

say "Starting the nginx SNI router on :$ROUTER_PORT"
# Some containers have no IPv6 stack; nginx refuses to start on a listen
# directive it cannot bind, so only emit it when the family works.
LISTEN6=""
if python3 -c 'import socket,sys; s=socket.socket(socket.AF_INET6); s.close()' 2>/dev/null; then
    LISTEN6="listen [::]:$ROUTER_PORT;"
fi
cat > "$WORK/nginx.conf" <<NGINX
load_module $(ls /usr/lib/nginx/modules/ngx_stream_module.so 2>/dev/null || echo modules/ngx_stream_module.so);
events {}
error_log $WORK/nginx-error.log warn;
pid $WORK/nginx.pid;
stream {
    map \$ssl_preread_server_name \$tenant {
        $ACME_HOST    127.0.0.1:$ACME_PORT;
        $GLOBEX_HOST  127.0.0.1:$GLOBEX_PORT;
        default       "";
    }
    server {
        listen $ROUTER_PORT;
        $LISTEN6
        ssl_preread on;
        proxy_pass \$tenant;
        proxy_timeout 1h;
    }
}
NGINX
nginx -t -c "$WORK/nginx.conf" >/dev/null 2>&1 || {
    nginx -t -c "$WORK/nginx.conf"; fail "router config invalid"; }
nginx -c "$WORK/nginx.conf"
NGINX_STARTED=1
wait_for 10 "router to accept connections" bash -c \
    "echo | timeout 2 openssl s_client -connect 127.0.0.1:$ROUTER_PORT -servername $ACME_HOST 2>/dev/null | grep -q CERTIFICATE"
pass "router listening"

say "1. The router hands each tenant its OWN certificate"
routed_fp() { # routed_fp HOSTNAME
    echo | openssl s_client -connect "127.0.0.1:$ROUTER_PORT" -servername "$1" 2>/dev/null \
        | openssl x509 -noout -fingerprint -sha256 2>/dev/null \
        | sed 's/.*=//; s/://g' | tr 'A-F' 'a-f'
}
[ "$(routed_fp "$ACME_HOST")" = "$ACME_FP" ] \
    || fail "SNI $ACME_HOST routed to the wrong tenant"
[ "$(routed_fp "$GLOBEX_HOST")" = "$GLOBEX_FP" ] \
    || fail "SNI $GLOBEX_HOST routed to the wrong tenant"
pass "SNI routing selects the right backend, certificate unmodified"

say "2. Enrolling an agent for acme, THROUGH the router"
api "$ACME_GUI" POST /api/admin/setup '{"Username":"acme-admin","Password":"correcthorse"}' >/dev/null
api "$GLOBEX_GUI" POST /api/admin/setup '{"Username":"globex-admin","Password":"correcthorse"}' >/dev/null
TOKEN=$(api "$ACME_GUI" POST /api/admin/tokens '{"Note":"e2e"}' | jq -r .token)
BASE_URL=$(api "$ACME_GUI" POST /api/admin/tokens '{"Note":"e2e2"}' | jq -r .base_url)
[ "$BASE_URL" = "https://$ACME_HOST:$ROUTER_PORT" ] \
    || fail "enrollment URL is $BASE_URL, expected the routed agent address"
pass "enrollment command points at the router, not the backend"

mkdir -p "$WORK/agent-state"
"$WORK/backup-agent" enroll --server "https://$ACME_HOST:$ROUTER_PORT" \
    --token "$TOKEN" --fingerprint "$ACME_FP" --name acme-client \
    --state-dir "$WORK/agent-state" >"$WORK/enroll.log" 2>&1 \
    || { cat "$WORK/enroll.log"; fail "enrollment through the router failed"; }
pass "agent enrolled through the router with the pin enforced"

say "3. A real backup over the routed connection"
"$WORK/backup-agent" run --state-dir "$WORK/agent-state" \
    --config "$WORK/agent-state/agent.yaml" >"$WORK/agent.log" 2>&1 &
AGENT_PID=$!
agent_online() { api "$ACME_GUI" GET /api/admin/agents | jq -e '.[0].online == true' >/dev/null; }
wait_for 20 "agent to come online" agent_online
AGENT_ID=$(api "$ACME_GUI" GET /api/admin/agents | jq -r '.[0].ID')
pass "agent online in acme: $AGENT_ID"

FIX="$WORK/fixture"; mkdir -p "$FIX"; echo "tenant acme data" > "$FIX/file.txt"
JOB_ID=$(api "$ACME_GUI" POST /api/admin/jobs \
    "{\"agent_id\":\"$AGENT_ID\",\"name\":\"e2e\",\"paths\":[\"$FIX\"],\"enabled\":true}" | jq -r '.id // .ID')
api "$ACME_GUI" POST "/api/admin/jobs/$JOB_ID/run" '{"mode":"full"}' >/dev/null
has_snapshot() { [ "$(api "$ACME_GUI" GET /api/admin/snapshots | jq 'length')" -gt 0 ]; }
wait_for 45 "snapshot to appear" has_snapshot
pass "backup completed over the SNI-routed connection"

say "4. Tenant isolation"
[ "$(api "$GLOBEX_GUI" GET /api/admin/agents | jq 'length')" -eq 0 ] \
    || fail "globex can see acme's agent"
[ "$(api "$GLOBEX_GUI" GET /api/admin/snapshots | jq 'length')" -eq 0 ] \
    || fail "globex can see acme's snapshots"
pass "globex sees no agents and no snapshots of acme's"

say "5. The router cannot be used to cross tenants"
# Same credentials, same router, but globex's hostname: the pin must reject
# the certificate globex presents.
sed "s|https://$ACME_HOST|https://$GLOBEX_HOST|" \
    "$WORK/agent-state/creds.json" > "$WORK/cross.json"
mkdir -p "$WORK/cross-state" && cp "$WORK/cross.json" "$WORK/cross-state/creds.json"
if timeout 20 "$WORK/backup-agent" run --state-dir "$WORK/cross-state" \
        --config "$WORK/cross-state/agent.yaml" >"$WORK/cross.log" 2>&1; then
    fail "agent connected to the wrong tenant — pinning did not hold"
fi
grep -qi "fingerprint mismatch" "$WORK/cross.log" \
    || { cat "$WORK/cross.log"; fail "expected a fingerprint mismatch"; }
pass "crossing tenants fails the pin: $(grep -io 'fingerprint mismatch.*' "$WORK/cross.log" | head -1 | cut -c1-60)…"

say "6. No SNI (IP literal) is refused, not misrouted"
# Go omits SNI for IP literals, so such an agent is unroutable. The map's
# `default ""` must refuse it rather than pick a tenant.
if echo | timeout 5 openssl s_client -connect "127.0.0.1:$ROUTER_PORT" -noservername 2>/dev/null \
        | grep -q "BEGIN CERTIFICATE"; then
    fail "a connection without SNI was routed to a tenant — the map is not failing closed"
fi
pass "connection without SNI refused by the router"

echo
echo "All $PASS_COUNT checks passed."
echo "Certificate pinning and tenant isolation hold across the SNI router."
