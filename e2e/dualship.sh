#!/usr/bin/env bash
#
# Fast, keyless dual-shipping check: no Datadog, no network. Run the agent against
# TWO local fake intakes (org A and org B), with org B configured as an
# additional_endpoints destination for both metrics and logs. Emit one DogStatsD
# metric and one log line, then assert the same telemetry lands in BOTH org
# recordings, each carrying that org's own API key. The key check is the real proof
# of dual-shipping: org A's recording must hold only KEY_A and org B's only KEY_B,
# for the header (series/logs) and for the v5 /intake/ body (host metadata), so a
# fan-out bug that reused one org's body or key for the other would fail here.
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
INTAKE_A=18091
INTAKE_B=18092
DSD_PORT=18127
KEY_A="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
KEY_B="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
HOST="dualship-host"
TOKEN="dualship-log-$$-${RANDOM}"
AGENT_PID=""
SERVE_PID=""

cleanup() {
	[ -n "$AGENT_PID" ] && kill -INT "$AGENT_PID" 2>/dev/null || true
	[ -n "$SERVE_PID" ] && kill -INT "$SERVE_PID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

echo "==> building agent + parity"
GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 go -C "$ROOT" build -tags netgo -o "$WORK/agent" ./cmd/agent
GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 go -C "$ROOT" build -tags netgo -o "$WORK/parity" ./e2e/parity

mkdir -p "$WORK/rec" "$WORK/confd/dualship.d" "$WORK/run"
: > "$WORK/app.log" # exists before startup so the tailer captures appends (tail-from-end)

# org A is the primary (dd_url + logs_dd_url). org B rides along as an additional
# endpoint for both the infra intake (additional_endpoints) and logs.
cat > "$WORK/datadog.yaml" <<EOF
api_key: ${KEY_A}
hostname: ${HOST}
dd_url: http://127.0.0.1:${INTAKE_A}
additional_endpoints:
  "http://127.0.0.1:${INTAKE_B}": ["${KEY_B}"]
dogstatsd_port: ${DSD_PORT}
enable_metadata_collection: true
logs_enabled: true
confd_path: $WORK/confd
run_path: $WORK/run
logs_config:
  logs_dd_url: http://127.0.0.1:${INTAKE_A}
  additional_endpoints:
    - api_key: ${KEY_B}
      host: 127.0.0.1
      port: ${INTAKE_B}
      use_ssl: false
EOF

cat > "$WORK/confd/dualship.d/conf.yaml" <<EOF
logs:
  - type: file
    path: $WORK/app.log
    service: dualship
    source: dualship
EOF

echo "==> starting two fake intakes + agent"
"$WORK/parity" serve -dir "$WORK/rec" a=127.0.0.1:${INTAKE_A} b=127.0.0.1:${INTAKE_B} > "$WORK/serve.log" 2>&1 &
SERVE_PID=$!
sleep 1
"$WORK/agent" --cfgpath "$WORK/datadog.yaml" --debug > "$WORK/agent.log" 2>&1 &
AGENT_PID=$!
sleep 2 # let host metadata ship and the file tailer reach EOF

echo "==> emitting a DogStatsD metric and a log line"
printf 'dualship.metric:42|g|#env:test' > "/dev/udp/127.0.0.1/${DSD_PORT}" || true
printf '%s line one\n' "$TOKEN" >> "$WORK/app.log"
sleep 6 # let the logs batch ship. The metric flushes on the shutdown drain below

echo "==> shutting down (final metrics flush)"
kill -INT "$AGENT_PID" 2>/dev/null || true; wait "$AGENT_PID" 2>/dev/null || true; AGENT_PID=""
kill -INT "$SERVE_PID" 2>/dev/null || true; wait "$SERVE_PID" 2>/dev/null || true; SERVE_PID=""

for f in a b; do
	if [ ! -s "$WORK/rec/$f.jsonl" ]; then
		echo "==> FAIL: org $f received nothing"
		tail -20 "$WORK/agent.log"
		exit 1
	fi
done

echo "==> verifying org A received the telemetry with KEY_A only"
"$WORK/parity" verify -series dualship.metric -log "$TOKEN" -meta -host "$HOST" -api-key "$KEY_A" "$WORK/rec/a.jsonl"

echo "==> verifying org B received the same telemetry with KEY_B only"
"$WORK/parity" verify -series dualship.metric -log "$TOKEN" -meta -host "$HOST" -api-key "$KEY_B" "$WORK/rec/b.jsonl"

echo "==> PASS: metrics, logs, and host metadata dual-shipped, each org keyed with its own API key"
