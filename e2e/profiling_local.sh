#!/usr/bin/env bash
#
# Fast, keyless profiling-proxy check: no Datadog, no network, no profiler. Start
# the agent's proxy pointed at the local fake intake (e2e/parity), POST a synthetic
# multipart upload, and assert the proxy forwarded it with the headers it injects.
# Proves the proxy end to end on any host, so CI can run it without secrets.
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
PORT=18126
INTAKE=18090
AGENT_PID=""
SERVE_PID=""

cleanup() {
	[ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null || true
	[ -n "$SERVE_PID" ] && kill "$SERVE_PID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

echo "==> building agent + parity"
GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 go -C "$ROOT" build -tags netgo -o "$WORK/agent" ./cmd/agent
GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 go -C "$ROOT" build -tags netgo -o "$WORK/parity" ./e2e/parity

mkdir -p "$WORK/rec"
cat > "$WORK/datadog.yaml" <<EOF
api_key: dummy
hostname: prof-local-host
env: staging
dogstatsd_port: 0
enable_metadata_collection: false
apm_config:
  enabled: true
  receiver_port: ${PORT}
  profiling_dd_url: http://127.0.0.1:${INTAKE}/api/v2/profile
EOF

# A synthetic upload shaped like a tracer's: an event.json part plus one pprof
# attachment. The bytes are opaque to the proxy, so they need not be real pprof.
cat > "$WORK/event.json" <<'EOF'
{"version":"4","family":"go","attachments":["cpu.pprof"],"tags_profiler":"service:prof-local,runtime:go"}
EOF
printf 'synthetic gzipped pprof bytes' > "$WORK/cpu.pprof"

echo "==> starting fake intake + agent"
"$WORK/parity" serve -dir "$WORK/rec" prof=127.0.0.1:${INTAKE} > "$WORK/serve.log" 2>&1 &
SERVE_PID=$!
sleep 1
"$WORK/agent" --cfgpath "$WORK/datadog.yaml" --debug > "$WORK/agent.log" 2>&1 &
AGENT_PID=$!
sleep 1

echo "==> posting a profile upload to the proxy"
code=$(curl -sS -o /dev/null -w '%{http_code}' \
	-F 'event=@'"$WORK"'/event.json;filename=event.json;type=application/json' \
	-F 'cpu.pprof=@'"$WORK"'/cpu.pprof;filename=cpu.pprof' \
	"http://127.0.0.1:${PORT}/profiling/v1/input")
echo "  proxy returned HTTP ${code}"

kill -INT "$AGENT_PID" 2>/dev/null || true; wait "$AGENT_PID" 2>/dev/null || true; AGENT_PID=""
kill -INT "$SERVE_PID" 2>/dev/null || true; wait "$SERVE_PID" 2>/dev/null || true; SERVE_PID=""

if [ "$code" != 200 ]; then
	echo "==> FAIL: proxy returned ${code}, want 200 (202 must be rewritten)"
	tail -10 "$WORK/agent.log"
	exit 1
fi

echo "==> verifying the forwarded profile"
"$WORK/parity" verify -profile -profile-family go -profile-attach cpu.pprof "$WORK/rec/prof.jsonl"
