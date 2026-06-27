#!/usr/bin/env bash
#
# Fast, keyless journald check: no Datadog, no network. Emit a uniquely tagged
# line into the systemd journal, run the agent with a journald log source pointed
# at the local fake intake (e2e/parity), and assert the structured entry was
# shipped. Proves the journald tailer end to end on any systemd host, so CI can
# run it without secrets. Skips cleanly where journald is absent or unreadable.
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v journalctl >/dev/null 2>&1; then
	echo "==> SKIP: journalctl not found (not a systemd host)"
	exit 0
fi
if ! journalctl -n0 -o json >/dev/null 2>&1; then
	echo "==> SKIP: cannot read the journal as this user"
	exit 0
fi
if ! command -v logger >/dev/null 2>&1; then
	echo "==> SKIP: logger not found, cannot write a test journal entry"
	exit 0
fi

WORK="$(mktemp -d)"
INTAKE=18080
AGENT_PID=""
SERVE_PID=""

cleanup() {
	[ -n "$AGENT_PID" ] && kill -INT "$AGENT_PID" 2>/dev/null || true
	[ -n "$SERVE_PID" ] && kill -INT "$SERVE_PID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

TAG="microagent_journald_$$_${RANDOM}"
TOKEN="journald-e2e-${TAG}"

echo "==> building agent + parity"
GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 go -C "$ROOT" build -tags netgo -o "$WORK/agent" ./cmd/agent
GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 go -C "$ROOT" build -tags netgo -o "$WORK/parity" ./e2e/parity

mkdir -p "$WORK/rec" "$WORK/confd/journald.d" "$WORK/run"
cat > "$WORK/datadog.yaml" <<EOF
api_key: dummy
hostname: journald-local-host
dd_url: http://127.0.0.1:${INTAKE}
dogstatsd_port: 0
enable_metadata_collection: false
logs_enabled: true
confd_path: $WORK/confd
run_path: $WORK/run
logs_config:
  logs_dd_url: http://127.0.0.1:${INTAKE}
EOF

# A unique SYSLOG_IDENTIFIER plus start_position beginning makes pickup
# deterministic: journalctl reads the whole journal but only our tagged lines
# match, so there is no follow race against the emit.
cat > "$WORK/confd/journald.d/conf.yaml" <<EOF
logs:
  - type: journald
    source: systemd
    start_position: beginning
    include_matches: ["SYSLOG_IDENTIFIER=${TAG}"]
EOF

echo "==> emitting tagged journal lines (info and error)"
logger -t "$TAG" "$TOKEN info line"
logger -t "$TAG" -p user.err "$TOKEN error line"
sleep 0.3

echo "==> starting fake intake + agent"
"$WORK/parity" serve -dir "$WORK/rec" ours=127.0.0.1:${INTAKE} > "$WORK/serve.log" 2>&1 &
SERVE_PID=$!
sleep 1
"$WORK/agent" --cfgpath "$WORK/datadog.yaml" --debug > "$WORK/agent.log" 2>&1 &
AGENT_PID=$!
sleep 3

echo "==> shutting down (ordered flush)"
kill -INT "$AGENT_PID" 2>/dev/null || true; wait "$AGENT_PID" 2>/dev/null || true; AGENT_PID=""
kill -INT "$SERVE_PID" 2>/dev/null || true; wait "$SERVE_PID" 2>/dev/null || true; SERVE_PID=""

if ! grep -q "log batch sent" "$WORK/agent.log"; then
	echo "==> FAIL: agent never reported a shipped log batch"
	tail -20 "$WORK/agent.log"
	exit 1
fi

echo "==> verifying the shipped journald entry"
"$WORK/parity" verify -log "$TOKEN" "$WORK/rec/ours.jsonl"

# Status parity: the error line (PRIORITY 3) must ship as status:error, the proof
# the PRIORITY-to-status mapping survives the round trip.
if ! grep -q '"status":"error"' "$WORK/rec/ours.jsonl"; then
	echo "==> FAIL: error-priority entry did not ship as status:error"
	exit 1
fi
echo "==> PASS: journald entry shipped with structured body and mapped status"
