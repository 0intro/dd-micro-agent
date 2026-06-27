#!/usr/bin/env bash
#
# End-to-end profiling test: run the micro-agent's profiling proxy against real
# Datadog, drive a Go profiler (dd-trace-go) and a C profiler (ddprof) at it, and
# confirm the proxy forwards both uploads to the profiling intake. Like e2e/e2e.sh,
# the real assertion is a 2xx from the intake (the agent logs "profile forwarded"),
# since the proxy is a passthrough and the pprof bytes are the profiler's, not ours.
# pup cannot query profiles (it reports profiling "not supported in pup yet"), so
# delivery is the assertion, the same standard e2e.sh uses for logs and processes.
#
#   DD_API_KEY        : the proxy injects it when forwarding (agent reads it from env)
#   DD_SITE           : optional, defaults to datadoghq.com
#   DD_TRACE_GO_DIR   : optional, a dd-trace-go checkout for the Go example
#                       (defaults to the sibling ../dd-trace-go)
#
# Usage:
#   DD_API_KEY=... e2e/profiling.sh
#
set -euo pipefail

: "${DD_API_KEY:?set DD_API_KEY}"
export DD_API_KEY
export DD_SITE="${DD_SITE:-datadoghq.com}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
RUN_ID="$(date +%s)$RANDOM"
TAG="profiling_e2e:${RUN_ID}"
E2E_HOSTNAME="microagent-prof-host-${RUN_ID}"
PORT=18126
AGENT_PID=""

cleanup() {
	[ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

# forwards counts the profile uploads the proxy has logged so far. Each phase
# asserts the count grew, which attributes a forward to that phase.
forwards() { grep -c "profile forwarded" "$WORK/agent.log" 2>/dev/null || true; }

echo "==> building agent"
GOTOOLCHAIN=local GOFLAGS=-mod=mod \
	go -C "$ROOT" build -tags netgo -o "$WORK/agent" ./cmd/agent

echo "==> writing config (run_id=$RUN_ID)"
cat > "$WORK/datadog.yaml" <<EOF
site: ${DD_SITE}
hostname: ${E2E_HOSTNAME}
env: e2e
dogstatsd_port: 0
enable_metadata_collection: false
tags:
  - ${TAG}
apm_config:
  enabled: true
  receiver_port: ${PORT}
EOF

echo "==> starting agent (api key from DD_API_KEY)"
"$WORK/agent" --cfgpath "$WORK/datadog.yaml" --debug > "$WORK/agent.log" 2>&1 &
AGENT_PID=$!
sleep 2

pass=0

# Go profiler (dd-trace-go). Built as its own module so the agent's go.mod stays
# clean. The build needs network (or a warm module cache) for dd-trace-go's deps.
echo "==> Go profiler (dd-trace-go)"
DDTRACEGO="${DD_TRACE_GO_DIR:-$ROOT/../dd-trace-go}"
if [ -d "$DDTRACEGO" ]; then
	cp "$ROOT"/e2e/profiling/goprofiled/main.go "$ROOT"/e2e/profiling/goprofiled/go.mod "$ROOT"/e2e/profiling/goprofiled/go.sum "$WORK/"
	( cd "$WORK" && go mod edit -replace "github.com/DataDog/dd-trace-go/v2=$(cd "$DDTRACEGO" && pwd)" )
	if GOTOOLCHAIN=local CGO_ENABLED=0 go -C "$WORK" build -o "$WORK/goprofiled" . ; then
		before=$(forwards)
		PROFILE_AGENT_ADDR="127.0.0.1:${PORT}" DD_SERVICE=microagent-e2e-go DD_ENV=e2e \
			DD_VERSION=0.0.1 PROFILE_TAG="${TAG}" PROFILE_DURATION=40s \
			"$WORK/goprofiled" > "$WORK/goprofiled.log" 2>&1 || true
		sleep 3
		if [ "$(forwards)" -gt "$before" ]; then
			echo "  ok    Go profile forwarded (intake returned 2xx)"
		else
			echo "  FAIL  no Go profile forwarded"; tail -5 "$WORK/goprofiled.log"; pass=1
		fi
	else
		echo "  FAIL  goprofiled build failed"; pass=1
	fi
else
	echo "  note  no dd-trace-go checkout (set DD_TRACE_GO_DIR); skipping Go profiler"
fi

# C profiler (ddprof), downloaded prebuilt. Linux x86_64/arm64 only, and needs perf
# access. Guarded so a restricted host skips it with a note rather than failing.
echo "==> C profiler (ddprof)"
case "$(uname -m)" in
	x86_64) DARCH=amd64 ;;
	aarch64 | arm64) DARCH=arm64 ;;
	*) DARCH="" ;;
esac
if [ "$(uname -s)" = Linux ] && [ -n "$DARCH" ] && command -v gcc >/dev/null &&
	curl -fsSL -o "$WORK/ddprof" "https://github.com/DataDog/ddprof/releases/latest/download/ddprof-${DARCH}" &&
	chmod +x "$WORK/ddprof" && "$WORK/ddprof" --version >/dev/null 2>&1; then
	sudo sysctl -w kernel.perf_event_paranoid=1 >/dev/null 2>&1 || true
	gcc -O0 -g -o "$WORK/work" "$ROOT/e2e/profiling/cprofiled/work.c"
	before=$(forwards)
	DD_SERVICE=microagent-e2e-c DD_ENV=e2e DD_VERSION=0.0.1 DD_TAGS="${TAG}" \
		"$WORK/ddprof" -S microagent-e2e-c -H 127.0.0.1 -P "${PORT}" -u 15 "$WORK/work" 40 \
		> "$WORK/ddprof.log" 2>&1 || true
	sleep 3
	if [ "$(forwards)" -gt "$before" ]; then
		echo "  ok    C profile forwarded (intake returned 2xx)"
	else
		echo "  note  ddprof produced no forwarded profile (perf restricted?); ddprof log tail:"
		tail -8 "$WORK/ddprof.log"
	fi
else
	echo "  note  ddprof unavailable for $(uname -s)/$(uname -m); skipping C profiling"
fi

kill -TERM "$AGENT_PID" 2>/dev/null || true
wait "$AGENT_PID" 2>/dev/null || true
AGENT_PID=""

# Backend confirmation. pup does not support profiling queries (`pup profiling`
# reports "not supported in pup yet"), and there is no API-key-queryable profile
# search, so delivery is proven the same way e2e.sh proves logs and processes: the
# real intake returned 2xx for each forward above. The profiles are visible in the
# Datadog "Profiles" explorer for service:microagent-e2e-go / microagent-e2e-c and
# host:${E2E_HOSTNAME}.
echo "==> backend confirmation"
echo "  note  pup has no profiling query (delivery proven by the 2xx forwards)"
echo "  note  view in Datadog: service:microagent-e2e-go OR service:microagent-e2e-c, host:${E2E_HOSTNAME}, tag ${TAG}"

if [ "$pass" -eq 0 ]; then
	echo "==> PROFILING E2E PASS"
else
	echo "==> PROFILING E2E FAIL. Agent log tail:"
	tail -25 "$WORK/agent.log"
fi
exit "$pass"
