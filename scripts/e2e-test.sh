#!/usr/bin/env bash
# End-to-end test: real server + real agent on localhost.
# Exercises: setup, login, enrollment (fingerprint pin), server-side job
# creation, remote "back up now" (full + incremental), client-side job
# config sync (both directions), restore to alternate dir, zip download,
# snapshot deletion and GC.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=18443
BASE="https://127.0.0.1:$PORT"
WORK="$(mktemp -d)"
JAR="$WORK/cookies.txt"
HDR=(-H 'X-Requested-With: fetch' -H 'Content-Type: application/json')
PASS_COUNT=0

cleanup() {
    kill "${AGENT_PID:-0}" "${SERVER_PID:-0}" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

say()  { echo -e "\n=== $*"; }
pass() { PASS_COUNT=$((PASS_COUNT+1)); echo "  ok: $*"; }
fail() { echo "  FAIL: $*" >&2; exit 1; }

capi() { # capi METHOD PATH [JSON_BODY]
    local method="$1" path="$2" body="${3:-}"
    if [ -n "$body" ]; then
        curl -ksS -b "$JAR" -c "$JAR" -X "$method" "${HDR[@]}" -d "$body" "$BASE$path"
    else
        curl -ksS -b "$JAR" -c "$JAR" -X "$method" "${HDR[@]}" "$BASE$path"
    fi
}

wait_for() { # wait_for SECONDS DESCRIPTION COMMAND...
    local deadline=$(( $(date +%s) + $1 )); shift
    local desc="$1"; shift
    while ! "$@" >/dev/null 2>&1; do
        [ "$(date +%s)" -lt "$deadline" ] || fail "timeout waiting for $desc"
        sleep 1
    done
}

say "Building binaries"
go build -o "$WORK/backup-server" ./cmd/server
go build -o "$WORK/backup-agent" ./cmd/agent

say "Starting server"
mkdir -p "$WORK/data"
CB_DATA_DIR="$WORK/data" CB_LISTEN="127.0.0.1:$PORT" \
    "$WORK/backup-server" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
wait_for 15 "server to listen" curl -ksSf "$BASE/login" -o /dev/null
pass "server is up"

say "First-run setup + login"
capi POST /api/admin/setup '{"username":"admin","password":"testpass123"}' | jq -e .username >/dev/null || fail "setup"
capi GET /api/admin/overview | jq -e .stats >/dev/null || fail "session works"
pass "admin account created, session valid"

say "Enrollment"
TOKEN_JSON=$(capi POST /api/admin/tokens '{"note":"e2e"}')
TOKEN=$(echo "$TOKEN_JSON" | jq -r .token)
FP=$(echo "$TOKEN_JSON" | jq -r .fingerprint)
[ -n "$TOKEN" ] && [ -n "$FP" ] || fail "token creation"

mkdir -p "$WORK/agent-state"
"$WORK/backup-agent" enroll --server "$BASE" --token "$TOKEN" --fingerprint "$FP" \
    --name e2e-client --state-dir "$WORK/agent-state" || fail "enroll"
# Token must be single-use.
if "$WORK/backup-agent" enroll --server "$BASE" --token "$TOKEN" \
    --state-dir "$WORK/agent-state2" 2>/dev/null; then
    fail "enrollment token was reusable"
fi
pass "enrolled with pinned fingerprint; token is single-use"

# Wrong fingerprint must be rejected.
if "$WORK/backup-agent" enroll --server "$BASE" --token whatever \
    --fingerprint "$(printf '0%.0s' {1..64})" --state-dir "$WORK/agent-state3" 2>/dev/null; then
    fail "wrong fingerprint accepted"
fi
pass "wrong fingerprint rejected"

say "Starting agent"
"$WORK/backup-agent" run --state-dir "$WORK/agent-state" \
    --config "$WORK/agent-state/agent.yaml" >"$WORK/agent.log" 2>&1 &
AGENT_PID=$!
agent_online() { capi GET /api/admin/agents | jq -e '.[0].online == true' >/dev/null; }
wait_for 15 "agent to come online" agent_online
AGENT_ID=$(capi GET /api/admin/agents | jq -r '.[0].ID')
pass "agent online: $AGENT_ID"

say "Fixture data"
FIX="$WORK/fixture"
mkdir -p "$FIX/sub" "$FIX/skipme"
echo "hello world"        > "$FIX/a.txt"
echo "same content"       > "$FIX/dup1.txt"
echo "same content"       > "$FIX/dup2.txt"     # dedup pair
head -c 300000 /dev/urandom > "$FIX/sub/big.bin"
echo "tempfile"           > "$FIX/junk.tmp"     # excluded by pattern
echo "excluded dir"       > "$FIX/skipme/x.txt" # excluded by dir pattern
ln -s a.txt "$FIX/link"

say "Create job from server GUI/API"
JOB=$(capi POST /api/admin/jobs "{\"agent_id\":\"$AGENT_ID\",\"name\":\"e2e\",\"paths\":[\"$FIX\"],\"excludes\":[\"*.tmp\",\"skipme\"],\"enabled\":true,\"keep_last\":0}")
JOB_ID=$(echo "$JOB" | jq -r .id)
[ -n "$JOB_ID" ] && [ "$JOB_ID" != null ] || fail "job create: $JOB"
pass "job $JOB_ID created"

# The job must be pushed into the agent's local agent.yaml.
yaml_has_job() { grep -q "id: $JOB_ID" "$WORK/agent-state/agent.yaml"; }
wait_for 10 "job to appear in agent.yaml" yaml_has_job
pass "server job synced into agent.yaml"

say "Full backup (remote run-now)"
RUN1=$(capi POST "/api/admin/jobs/$JOB_ID/run" '{"mode":"full"}' | jq -r .run_id)
run_status() { capi GET "/api/admin/runs/$1" | jq -r .run.Status; }
run_finished() { [[ "$(run_status "$1")" =~ ^(success|partial|error)$ ]]; }
wait_for 30 "full run to finish" run_finished "$RUN1"
[ "$(run_status "$RUN1")" = success ] || { cat "$WORK/agent.log"; fail "full run: $(capi GET /api/admin/runs/$RUN1)"; }
SNAP1=$(capi GET "/api/admin/runs/$RUN1" | jq -r .run.SnapshotID)
STATS1=$(capi GET "/api/admin/runs/$RUN1" | jq -r .run.Stats)
UP1=$(echo "$STATS1" | jq -r .bytes_uploaded)
FILES1=$(echo "$STATS1" | jq -r .files_total)
# 4 regular files (a.txt, dup1, dup2, big.bin) — junk.tmp and skipme/
# excluded, symlink recorded separately.
[ "$FILES1" = 4 ] || fail "expected 4 files in snapshot, got $FILES1 ($STATS1)"
[ "$UP1" -gt 300000 ] || fail "full upload too small: $UP1"
pass "full backup: snapshot $SNAP1, $FILES1 files, uploaded $UP1 bytes (excludes honoured)"

say "Incremental backup after small change"
echo "changed!" >> "$FIX/a.txt"
RUN2=$(capi POST "/api/admin/jobs/$JOB_ID/run" '{"mode":"incremental"}' | jq -r .run_id)
wait_for 30 "incremental run to finish" run_finished "$RUN2"
[ "$(run_status "$RUN2")" = success ] || fail "incremental run failed"
STATS2=$(capi GET "/api/admin/runs/$RUN2" | jq -r .run.Stats)
UP2=$(echo "$STATS2" | jq -r .bytes_uploaded)
CHANGED2=$(echo "$STATS2" | jq -r .files_changed)
[ "$CHANGED2" = 1 ] || fail "expected 1 changed file, got $CHANGED2 ($STATS2)"
[ "$UP2" -lt 1000 ] || fail "incremental uploaded too much: $UP2"
SNAP2=$(capi GET "/api/admin/runs/$RUN2" | jq -r .run.SnapshotID)
pass "incremental: only changed file uploaded ($UP2 bytes vs $UP1 full)"

say "Snapshot browse + zip download"
TREE=$(capi GET "/api/admin/snapshots/$SNAP2/tree?path=")
echo "$TREE" | jq -e 'length >= 1' >/dev/null || fail "tree browse: $TREE"
curl -ksS -b "$JAR" -o "$WORK/dl.zip" "$BASE/api/admin/snapshots/$SNAP2/download?zip=1"
python3 -c "
import zipfile, sys
z = zipfile.ZipFile('$WORK/dl.zip')
names = z.namelist()
assert any(n.endswith('a.txt') for n in names), names
data = z.read([n for n in names if n.endswith('a.txt')][0]).decode()
assert 'changed!' in data, data
" || fail "zip download content"
pass "tree browse and zip download OK"

say "Restore to alternate directory"
REST="$WORK/restored"
RRUN=$(capi POST "/api/admin/snapshots/$SNAP2/restore" "{\"paths\":[],\"target_dir\":\"$REST\",\"overwrite\":true}" | jq -r .run_id)
wait_for 30 "restore run to finish" run_finished "$RRUN"
[ "$(run_status "$RRUN")" = success ] || fail "restore run failed: $(capi GET /api/admin/runs/$RRUN)"
RESTORED_ROOT="$REST${FIX}"   # re-rooted under target dir
diff -r --no-dereference "$FIX" "$RESTORED_ROOT" >"$WORK/diff.out" 2>&1 || {
    # junk.tmp and skipme were excluded from backup, so they may only exist in FIX
    grep -vE 'junk\.tmp|skipme' "$WORK/diff.out" | grep -q . && { cat "$WORK/diff.out"; fail "restored data differs"; }
}
cmp "$FIX/sub/big.bin" "$RESTORED_ROOT/sub/big.bin" || fail "big.bin corrupt after restore"
[ "$(readlink "$RESTORED_ROOT/link")" = "a.txt" ] || fail "symlink not restored"
pass "restore matches source (incl. 300KB binary + symlink)"

say "Client-side job config (agent.yaml -> server)"
"$WORK/backup-agent" job add --name local-job --path "$FIX/sub" \
    --state-dir "$WORK/agent-state" --config "$WORK/agent-state/agent.yaml"
local_job_on_server() { capi GET "/api/admin/jobs?agent=$AGENT_ID" | jq -e '.[] | select(.name=="local-job" and .origin=="client")' >/dev/null; }
wait_for 15 "client job to sync to server" local_job_on_server
LOCAL_JOB_ID=$(capi GET "/api/admin/jobs?agent=$AGENT_ID" | jq -r '.[] | select(.name=="local-job") | .id')
pass "client-created job synced to server as $LOCAL_JOB_ID"

say "Server edit flows back into agent.yaml"
capi POST /api/admin/jobs "{\"id\":\"$LOCAL_JOB_ID\",\"agent_id\":\"$AGENT_ID\",\"name\":\"local-job\",\"paths\":[\"$FIX/sub\"],\"schedule\":\"0 3 * * *\",\"enabled\":true}" >/dev/null
yaml_has_schedule() { grep -q '0 3 \* \* \*' "$WORK/agent-state/agent.yaml"; }
wait_for 10 "server edit to reach agent.yaml" yaml_has_schedule
pass "server-side edit written back to agent.yaml"

say "Client-side deletion (backup-agent job rm)"
"$WORK/backup-agent" job rm local-job \
    --state-dir "$WORK/agent-state" --config "$WORK/agent-state/agent.yaml"
local_job_gone() { ! local_job_on_server; }
wait_for 15 "client deletion to sync" local_job_gone
pass "client-side job deletion synced to server"

say "Snapshot deletion + GC"
capi DELETE "/api/admin/snapshots/$SNAP1" | jq -e .ok >/dev/null || fail "snapshot delete"
capi POST /api/admin/gc | jq -e .ok >/dev/null || fail "gc"
capi GET "/api/admin/snapshots?job=$JOB_ID" | jq -e "length == 1" >/dev/null || fail "snapshot count after delete"
pass "snapshot deleted, GC ran"

say "GUI pages render"
for p in /login /; do
    curl -ksSf -b "$JAR" "$BASE$p" | grep -qi "central backup" || fail "page $p"
done
pass "GUI pages render"

echo
echo "ALL E2E CHECKS PASSED ($PASS_COUNT checks)"
