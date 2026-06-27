#!/usr/bin/env bash
#
# End-to-end test: run the real agent against real Datadog, then verify with the
# `pup` Datadog CLI that the data arrived. Credentials come from the environment
# and are never written to disk:
#
#   DD_API_KEY  : submits metrics/logs (the agent reads it directly)
#   DD_APP_KEY  : lets pup query them back
#   DD_SITE     : optional, defaults to datadoghq.com
#
# Usage:
#   DD_API_KEY=... DD_APP_KEY=... e2e/e2e.sh
#
set -euo pipefail

: "${DD_API_KEY:?set DD_API_KEY}"
: "${DD_APP_KEY:?set DD_APP_KEY}"
export DD_API_KEY DD_APP_KEY
export DD_SITE="${DD_SITE:-datadoghq.com}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
RUN_ID="$(date +%s)$RANDOM"
TAG="e2e_run:${RUN_ID}"
TOKEN="microagent_e2e_${RUN_ID}"
E2E_HOSTNAME="microagent-e2e-host-${RUN_ID}" # unique, so we never touch a real host record
PORT=18125
AGENT_PID=""

cleanup() {
	[ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

# Isolate pup's config so it authenticates with DD_API_KEY/DD_APP_KEY for DD_SITE,
# rather than reusing a stored OAuth session that might point at a different org.
# Verification must read back from the same org the data was sent to.
export XDG_CONFIG_HOME="$WORK/pup"
mkdir -p "$XDG_CONFIG_HOME"

echo "==> building agent"
GOTOOLCHAIN=local GOFLAGS=-mod=mod \
	go -C "$ROOT" build -tags netgo -o "$WORK/agent" ./cmd/agent

echo "==> writing config (run_id=$RUN_ID)"
mkdir -p "$WORK/confd/e2e.d" "$WORK/run"
cat > "$WORK/datadog.yaml" <<EOF
site: ${DD_SITE}
hostname: ${E2E_HOSTNAME}
dogstatsd_port: ${PORT}
logs_enabled: true
confd_path: ${WORK}/confd
run_path: ${WORK}/run
tags:
  - ${TAG}
logs_config:
  batch_wait: 1
process_config:
  process_collection:
    enabled: true
EOF
cat > "$WORK/confd/e2e.d/conf.yaml" <<EOF
logs:
  - type: file
    path: ${WORK}/app.log
    service: microagent-e2e
    source: microagent-e2e
  - type: file
    path: ${WORK}/multiline.log
    service: microagent-e2e-ml
    source: microagent-e2e
    log_processing_rules:
      - type: multi_line
        name: ts_start
        pattern: '\d{4}-\d{2}-\d{2}'
EOF
: > "$WORK/app.log"
: > "$WORK/multiline.log"

echo "==> starting agent (api key from DD_API_KEY)"
"$WORK/agent" --cfgpath "$WORK/datadog.yaml" --debug > "$WORK/agent.log" 2>&1 &
AGENT_PID=$!
sleep 2

echo "==> submitting unique metrics and logs"
for _ in 1 2 3; do echo "microagent.e2e.gauge:42|g" > "/dev/udp/127.0.0.1/${PORT}"; done
echo "microagent.e2e.count:7|c" > "/dev/udp/127.0.0.1/${PORT}"
# histogram (1..10), timing, and set (3 distinct of 4) exercise the aggregation expansion.
# _sc and _e exercise the service-check and event intakes.
for v in 1 2 3 4 5 6 7 8 9 10; do echo "microagent.e2e.hist:${v}|h" > "/dev/udp/127.0.0.1/${PORT}"; done
echo "microagent.e2e.timing:250|ms" > "/dev/udp/127.0.0.1/${PORT}"
for u in alice bob alice carol; do echo "microagent.e2e.set:${u}|s" > "/dev/udp/127.0.0.1/${PORT}"; done
printf '_sc|microagent.e2e.check|0|#%s\n' "$TAG" > "/dev/udp/127.0.0.1/${PORT}"
EV_TITLE="microagent e2e event"; EV_TEXT="$TOKEN"
printf '_e{%d,%d}:%s|%s|t:info|#%s\n' "${#EV_TITLE}" "${#EV_TEXT}" "$EV_TITLE" "$EV_TEXT" "$TAG" > "/dev/udp/127.0.0.1/${PORT}"
echo "hello from ${TOKEN} line one" >> "$WORK/app.log"
echo "hello from ${TOKEN} line two" >> "$WORK/app.log"
# A multi-line entry (1 start line + 2 continuations) should aggregate into one message.
printf '2026-01-01 multiline %s\n  at frame one\n  at frame two\n' "$TOKEN" >> "$WORK/multiline.log"

sleep 3                                    # let the 1s log batch flush
kill -TERM "$AGENT_PID" 2>/dev/null || true; wait "$AGENT_PID" 2>/dev/null || true  # final metrics flush
AGENT_PID=""

# verification via pup (ingestion has latency, so retry)

metric_present() {
	pup metrics query --query="avg:$1{${TAG}}" --from=15m --output json 2>/dev/null \
		| jq -e '.data.series | length > 0' >/dev/null
}
gauge_is_42() {
	pup metrics query --query="avg:microagent.e2e.gauge{${TAG}}" --from=15m --output json 2>/dev/null \
		| jq -e '[.data.series[]?.pointlist[]?[1] | select(. != null)] | any(. == 42)' >/dev/null
}
logs_searchable() {
	pup logs search --query="service:microagent-e2e ${TOKEN}" --from=20m --limit=10 --output json 2>/dev/null \
		| jq -e '.data | length > 0' >/dev/null
}
host_tagged() {
	# The host-tags API reflects the host-tags from the v5 payload. (The hosts-list
	# API is gated by this org's "Data Access for hosts" policy.)
	pup api "v1/tags/hosts/${E2E_HOSTNAME}" 2>/dev/null \
		| jq -e --arg t "$TAG" '.tags | index($t)' >/dev/null
}

# wait_for retries cmd until it succeeds or $timeout seconds pass.
wait_for() {
	local desc=$1 timeout=$2; shift 2
	local waited=0
	until "$@"; do
		waited=$((waited + 10))
		if [ "$waited" -gt "$timeout" ]; then
			echo "  FAIL  $desc (timed out after ${waited}s)"
			return 1
		fi
		sleep 10
	done
	echo "  ok    $desc"
}

echo "==> verifying via pup (metric ingestion can take a few minutes)"
pass=0

# Metrics, queried back through pup. The gauge check proves both presence and the
# value. system.mem.total proves the host-collection path (cpu/net are rates that
# need a second collect, so they don't appear in this short run).
wait_for "dogstatsd gauge = 42"      300 gauge_is_42                         || pass=1
wait_for "dogstatsd counter present" 300 metric_present microagent.e2e.count || pass=1
wait_for "host metric present"       300 metric_present system.mem.total     || pass=1
# histogram/timing expand to sub-metrics. Set ships a distinct-member count gauge.
wait_for "dogstatsd histogram .avg present"     300 metric_present microagent.e2e.hist.avg          || pass=1
wait_for "dogstatsd histogram .95percentile"    300 metric_present microagent.e2e.hist.95percentile || pass=1
wait_for "dogstatsd timing .avg present"        300 metric_present microagent.e2e.timing.avg        || pass=1
wait_for "dogstatsd set distinct-count present" 300 metric_present microagent.e2e.set               || pass=1

# Logs: delivery is the real assertion. The agent logged a successful 2xx send.
if grep -q "log batch sent" "$WORK/agent.log"; then
	echo "  ok    logs delivered (intake returned 2xx)"
else
	echo "  FAIL  logs not delivered"
	pass=1
fi
# Searchability is best-effort: whether ingested logs are queryable depends on the
# org's log indexes (which may drop arbitrary services), so note but don't fail.
found=0
for _ in $(seq 1 9); do
	if logs_searchable; then found=1; break; fi
	sleep 10
done
if [ "$found" -eq 1 ]; then
	echo "  ok    logs searchable via pup"
else
	echo "  note  logs accepted but not searchable in this org's log indexes (delivery confirmed above)"
fi

# Host metadata: the agent posts a v5 payload to /intake/ at startup. Delivery is
# confirmed agent-side, and the host's tags are read back via pup.
if grep -q "host metadata sent" "$WORK/agent.log"; then
	echo "  ok    host metadata delivered (intake returned 2xx)"
else
	echo "  FAIL  host metadata not delivered"
	pass=1
fi
wait_for "host in infra backend with tag (pup)" 300 host_tagged || pass=1

# Service checks and events: delivery is the assertion (the aggregator logs on a 2xx),
# which confirms the check_run array and the public events API accept our wire formats.
if grep -q "service checks sent" "$WORK/agent.log"; then echo "  ok    service check delivered (2xx)"; else echo "  FAIL  service check not delivered"; pass=1; fi
if grep -q "events sent" "$WORK/agent.log"; then echo "  ok    event delivered (2xx)"; else echo "  FAIL  event not delivered"; pass=1; fi

# Multiline: the 3-line entry must ship as ONE message. Best-effort search (same indexing
# caveat as logs above) for a single message holding both the start line and the last
# continuation. Delivery itself is covered by 'log batch sent'. Aggregation is unit-tested.
ml_found=0
for _ in $(seq 1 6); do
	if pup logs search --query="service:microagent-e2e-ml ${TOKEN}" --from=20m --limit=5 --output json 2>/dev/null \
		| jq -e '[.data[]? | select((.attributes.message // "") | contains("frame two"))] | length > 0' >/dev/null; then ml_found=1; break; fi
	sleep 10
done
if [ "$ml_found" -eq 1 ]; then echo "  ok    multiline entry aggregated into one message"; else echo "  note  multiline not searchable in this org (aggregation is unit-tested; delivery confirmed)"; fi

# Live Processes: the agent posts protobuf CollectorProc payloads to process.<site>
# at startup. A 2xx from that intake is the real assertion (and it proves the
# hand-rolled, uncompressed-protobuf wire is accepted). The Live Processes view is
# host-scoped, so confirm visually for ${E2E_HOSTNAME} if needed.
if grep -q "process payload sent" "$WORK/agent.log"; then
	echo "  ok    process payloads delivered (process intake returned 2xx)"
else
	echo "  FAIL  process payloads not delivered"
	pass=1
fi

if [ "$pass" -eq 0 ]; then
	echo "==> E2E PASS"
else
	echo "==> E2E FAIL. Agent log tail:"
	tail -25 "$WORK/agent.log"
fi
exit "$pass"
