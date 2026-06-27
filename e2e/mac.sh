#!/usr/bin/env bash
#
# Full end-to-end test on macOS, run natively (no VM). macOS cannot be virtualized
# off Apple hardware under its license, so instead of the vm_*.sh pattern this runs
# the cross-built agent directly on the host, which on CI is a GitHub-hosted macOS
# runner (real Apple hardware). It runs the agent (metrics + logs + host metadata +
# Live Processes) as the current user, drives a DogStatsD sample plus an nginx-format
# access log, and verifies that the DogStatsD metrics, the macOS host metrics
# (system.load.* via vm.loadavg, system.disk.* via getfsstat, system.uptime via
# kern.boottime), the logs, the host metadata (platform darwin), and the process list
# all arrived.
#
# This is the live proof for the darwin host-stats path and the darwin process
# collector, which shells out to `ps` (internal/process/collect_darwin.go). The `ps`
# output format is the one part that can drift between macOS releases, so running on a
# real, current macOS is what validates it. macOS has no cpu or memory host metric
# (Mach host_statistics needs cgo), so those are not asserted.
#
# Two modes. With DD_API_KEY + DD_APP_KEY set it posts to real Datadog and verifies
# with pup (a manual run on a Mac). With DD_API_KEY unset it posts to a local fake
# intake (e2e/parity) and verifies with `parity verify` (the automated mode: no keys,
# no pup, no network to Datadog). CI uses the fake mode.
#
#   DD_API_KEY / DD_APP_KEY   set for real Datadog + pup, unset for the fake intake
#   DD_SITE                   defaults to datadoghq.com (real mode only)
#   STOP_AFTER                debug knob: provision | traffic | verify (default)
#
# Needs Go and a Bourne shell. No qemu, no network to Datadog in the fake mode. Written
# for the macOS system bash (3.2) and BSD userland, so no bashisms past 3.2 and no
# GNU-only tool flags.
set -uo pipefail

FAKE=0
[ -z "${DD_API_KEY:-}" ] && FAKE=1
if [ "$FAKE" = 0 ]; then
	: "${DD_APP_KEY:?set DD_APP_KEY (or unset DD_API_KEY to use the fake-intake mode)}"
	export DD_API_KEY DD_APP_KEY
fi
export DD_SITE="${DD_SITE:-datadoghq.com}"
STOP_AFTER="${STOP_AFTER:-verify}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
W="$(mktemp -d 2>/dev/null || mktemp -d -t microagent-mac)"
INTAKE=18080

RUN_ID="$(date +%s)$RANDOM"
HOST_NAME="microagent-mac-e2e-${RUN_ID}"
RUN_TAG="e2e_run:${RUN_ID}"
TEST_TAG="test:microagent-e2e"
AGENT_PID=""
SERVE_PID=""

PUP_HOME="$W/pup"
ddpup() { HOME="$PUP_HOME" XDG_CONFIG_HOME="$PUP_HOME" pup "$@"; }

log() { echo "==> $*"; }

cleanup() {
	log "cleanup"
	[ -n "$AGENT_PID" ] && kill -INT "$AGENT_PID" 2>/dev/null
	[ -n "$SERVE_PID" ] && kill -INT "$SERVE_PID" 2>/dev/null
	rm -rf "$W"
}
trap cleanup EXIT

stop_here() { if [ "$STOP_AFTER" = "$1" ]; then log "STOP_AFTER=$1 reached"; exit 0; fi; }

command -v go >/dev/null || { echo "go missing"; exit 1; }
if [ "$FAKE" = 0 ]; then
	command -v pup >/dev/null || { echo "pup missing (needed for real-Datadog mode)"; exit 1; }
	command -v jq >/dev/null || { echo "jq missing"; exit 1; }
	mkdir -p "$PUP_HOME"
fi
log "run_id=$RUN_ID host=$HOST_NAME os=$(uname -sm) fake=$FAKE"

log "building agent + dsdsample$([ "$FAKE" = 1 ] && echo ' + parity') (native)"
gobuild() { GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 go -C "$ROOT" build -tags netgo -o "$1" "$2"; }
gobuild "$W/agent" ./cmd/agent       || { echo "agent build failed"; exit 1; }
gobuild "$W/dsdsample" ./e2e/dsdsample || { echo "dsdsample build failed"; exit 1; }
[ "$FAKE" = 1 ] && { gobuild "$W/parity" ./e2e/parity || { echo "parity build failed"; exit 1; }; }

mkdir -p "$W/conf.d/nginx.d" "$W/run" "$W/rec"
: > "$W/access.log"
if [ "$FAKE" = 1 ]; then
	cat > "$W/datadog.yaml" <<EOF
api_key: dummy
dd_url: http://127.0.0.1:${INTAKE}
hostname: ${HOST_NAME}
tags:
  - ${TEST_TAG}
  - ${RUN_TAG}
logs_enabled: true
enable_metadata_collection: true
run_path: ${W}/run
confd_path: ${W}/conf.d
logs_config: {logs_dd_url: http://127.0.0.1:${INTAKE}}
process_config:
  process_dd_url: http://127.0.0.1:${INTAKE}
  process_collection:
    enabled: true
EOF
else
	cat > "$W/datadog.yaml" <<EOF
api_key: ${DD_API_KEY}
site: ${DD_SITE}
hostname: ${HOST_NAME}
tags:
  - ${TEST_TAG}
  - ${RUN_TAG}
logs_enabled: true
enable_metadata_collection: true
run_path: ${W}/run
confd_path: ${W}/conf.d
process_config:
  process_collection:
    enabled: true
EOF
fi
cat > "$W/conf.d/nginx.d/conf.yaml" <<EOF
logs:
  - type: file
    path: ${W}/access.log
    service: nginx
    source: nginx
EOF

# Start the fake intake (fake mode) before the agent so it captures startup metadata.
if [ "$FAKE" = 1 ]; then
	"$W/parity" serve -dir "$W/rec" ours=127.0.0.1:${INTAKE} > "$W/parity.log" 2>&1 &
	SERVE_PID=$!
	sleep 1
fi
"$W/agent" --cfgpath "$W/datadog.yaml" --debug > "$W/agent.log" 2>&1 &
AGENT_PID=$!
sleep 3
log "agent log (head):"; head -12 "$W/agent.log"
stop_here provision

# traffic. The access log is appended after the agent is tailing (it starts at EOF),
# and the DogStatsD sample runs long enough to span an agent flush.
log "writing nginx-format access log + running dogstatsd sample"
i=1
while [ "$i" -le 25 ]; do
	printf '127.0.0.1 - - [%s] "GET / HTTP/1.1" 200 12 "-" "e2e"\n' "$(date '+%d/%b/%Y:%H:%M:%S %z')" >> "$W/access.log"
	i=$((i + 1))
done
printf '127.0.0.1 - - [%s] "GET /microagent-e2e-%s HTTP/1.1" 200 5 "-" "e2e"\n' "$(date '+%d/%b/%Y:%H:%M:%S %z')" "$RUN_ID" >> "$W/access.log"
"$W/dsdsample" -addr 127.0.0.1:8125 -duration 25s -tags "${RUN_TAG},${TEST_TAG}" -prefix microagent.mac.dsd
sleep 8
log "agent log (tail):"; tail -12 "$W/agent.log"
stop_here traffic

if [ "$FAKE" = 1 ]; then
	log "stopping agent + recorder (final flush)"
	kill -INT "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null; AGENT_PID=""
	kill -INT "$SERVE_PID" 2>/dev/null; wait "$SERVE_PID" 2>/dev/null; SERVE_PID=""
	log "records: $(wc -l < "$W/rec/ours.jsonl" 2>/dev/null)"
	pass=0
	if ! "$W/parity" verify \
		-series datadog.agent.running,microagent.mac.dsd.gauge,microagent.mac.dsd.requests,microagent.mac.dsd.render.95percentile,microagent.mac.dsd.latency.avg,microagent.mac.dsd.users,system.load.1,system.disk.total,system.uptime \
		-check microagent.mac.dsd.check -event 'dsdsample up' \
		-platform darwin -meta -host "${HOST_NAME}" \
		-min-procs 10 -proc-name agent \
		-log "microagent-e2e-${RUN_ID}" \
		"$W/rec/ours.jsonl"; then
		pass=1
	fi
	# The rest of the pipeline (checks, events, metadata, logs, processes) is verified by
	# the agent's own 2xx debug lines, which fire because the fake intake returns 202.
	for line in 'host metadata sent' 'log batch sent' 'service checks sent' 'events sent' 'process payload sent'; do
		if grep -q "$line" "$W/agent.log"; then echo "  ok    delivered: $line"; else echo "  FAIL  not delivered: $line"; pass=1; fi
	done
	if [ "$pass" = 0 ]; then echo "==> MAC E2E (fake intake) PASS"; exit 0; fi
	echo "==> MAC E2E (fake intake) FAIL"
	echo "--- agent log tail ---"; tail -20 "$W/agent.log" 2>/dev/null
	exit 1
fi

# real Datadog + pup
metric_present() { ddpup metrics query --query="avg:$1{${RUN_TAG}}" --from=15m --output json 2>/dev/null | jq -e '.data.series | length > 0' >/dev/null; }
metric_is_42() { ddpup metrics query --query="avg:microagent.mac.dsd.gauge{${RUN_TAG}}" --from=15m --output json 2>/dev/null | jq -e '[.data.series[]?.pointlist[]?[1]|select(.!=null)]|any(.==42)' >/dev/null; }
host_present() { ddpup infrastructure hosts list --filter="host:${HOST_NAME}" --output json 2>/dev/null | jq -e --arg t "$RUN_TAG" '.data.host_list[0]|(.tags_by_source.Datadog|index($t)) and ((.meta.agent_version // "")!="")' >/dev/null; }
logs_searchable() { ddpup logs search --query="service:nginx ${RUN_TAG}" --from=20m --limit=10 --output json 2>/dev/null | jq -e '.data|length>0' >/dev/null; }
wait_for() { desc=$1; timeout=$2; shift 2; waited=0; until "$@"; do waited=$((waited+10)); if [ "$waited" -gt "$timeout" ]; then echo "  FAIL  $desc (timeout ${waited}s)"; return 1; fi; sleep 10; done; echo "  ok    $desc"; }

log "verifying via pup (ingestion latency ~minutes)"
pass=0
wait_for "dogstatsd gauge = 42"                 300 metric_is_42 || pass=1
wait_for "dogstatsd counter present"            300 metric_present microagent.mac.dsd.requests || pass=1
wait_for "dogstatsd set distinct-count present" 300 metric_present microagent.mac.dsd.users || pass=1
wait_for "host metric system.load.1"            300 metric_present system.load.1 || pass=1
wait_for "host metric system.disk.total"        300 metric_present system.disk.total || pass=1
wait_for "host metric system.uptime"            300 metric_present system.uptime || pass=1
for line in 'host metadata sent' 'log batch sent' 'service checks sent' 'events sent' 'process payload sent'; do
	if grep -q "$line" "$W/agent.log"; then echo "  ok    delivered: $line"; else echo "  FAIL  not delivered: $line"; pass=1; fi
done
hp=0; n=0; while [ "$n" -lt 18 ]; do host_present && { hp=1; break; }; sleep 10; n=$((n+1)); done
[ "$hp" = 1 ] && echo "  ok    host in Infrastructure List" || echo "  ..    host pending (indexing lag)"
wait_for "logs searchable via pup" 300 logs_searchable || pass=1
PROC_JSON="$W/processes.json"
curl -s -m 30 -H "DD-API-KEY: ${DD_API_KEY}" -H "DD-APPLICATION-KEY: ${DD_APP_KEY}" "https://api.${DD_SITE}/api/v2/processes?page%5Blimit%5D=1000" >"$PROC_JSON" 2>/dev/null
N=$(jq --arg h "$HOST_NAME" '[.data[]?|select(.attributes.host==$h)]|length' "$PROC_JSON" 2>/dev/null || echo 0)
if [ "${N:-0}" -ge 10 ]; then echo "  ok    Live Processes shows $N processes"; else echo "  FAIL  Live Processes shows ${N:-0} (<10)"; pass=1; fi
if jq -e --arg h "$HOST_NAME" 'any(.data[]?; .attributes.host==$h and (.attributes.tags|index("os_name:darwin")))' "$PROC_JSON" >/dev/null 2>&1; then echo "  ok    process os_name:darwin present"; else echo "  FAIL  process os_name:darwin missing"; pass=1; fi

if [ "$pass" = 0 ]; then echo "==> MAC E2E PASS"; else echo "==> MAC E2E FAIL"; fi
exit "$pass"
