#!/usr/bin/env bash
#
# End-to-end journald test against real Datadog. Emit a uniquely tagged line into
# the systemd journal, run the real agent with a journald log source, and confirm
# the entry reached Datadog. Credentials come from the environment, never written
# to disk:
#
#   DD_API_KEY  : submits logs (the agent reads it directly)
#   DD_APP_KEY  : lets pup query them back
#   DD_SITE     : optional, defaults to datadoghq.com
#
# Usage:
#   DD_API_KEY=... DD_APP_KEY=... e2e/journald.sh
#
set -euo pipefail

: "${DD_API_KEY:?set DD_API_KEY}"
: "${DD_APP_KEY:?set DD_APP_KEY}"
export DD_API_KEY DD_APP_KEY
export DD_SITE="${DD_SITE:-datadoghq.com}"

if ! command -v journalctl >/dev/null 2>&1 || ! journalctl -n0 -o json >/dev/null 2>&1; then
	echo "==> SKIP: no readable systemd journal on this host"
	exit 0
fi
if ! command -v logger >/dev/null 2>&1; then
	echo "==> SKIP: logger not found, cannot write a test journal entry"
	exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
RUN_ID="$(date +%s)$RANDOM"
TAG="microagent_journald_${RUN_ID}"
TOKEN="journald_e2e_${RUN_ID}"
E2E_HOSTNAME="microagent-journald-host-${RUN_ID}"
AGENT_PID=""

cleanup() {
	[ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

# Isolate pup's config so it authenticates with DD_API_KEY/DD_APP_KEY for DD_SITE.
export XDG_CONFIG_HOME="$WORK/pup"
mkdir -p "$XDG_CONFIG_HOME"

echo "==> building agent"
GOTOOLCHAIN=local GOFLAGS=-mod=mod \
	go -C "$ROOT" build -tags netgo -o "$WORK/agent" ./cmd/agent

echo "==> writing config (run_id=$RUN_ID)"
mkdir -p "$WORK/confd/journald.d" "$WORK/run"
cat > "$WORK/datadog.yaml" <<EOF
site: ${DD_SITE}
hostname: ${E2E_HOSTNAME}
dogstatsd_port: 0
enable_metadata_collection: false
logs_enabled: true
confd_path: ${WORK}/confd
run_path: ${WORK}/run
tags:
  - e2e_run:${RUN_ID}
logs_config:
  batch_wait: 1
EOF
# A unique SYSLOG_IDENTIFIER plus start_position beginning makes pickup
# deterministic, the same trick as journald_local.sh.
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

echo "==> starting agent (api key from DD_API_KEY)"
"$WORK/agent" --cfgpath "$WORK/datadog.yaml" --debug > "$WORK/agent.log" 2>&1 &
AGENT_PID=$!
sleep 3
kill -TERM "$AGENT_PID" 2>/dev/null || true; wait "$AGENT_PID" 2>/dev/null || true
AGENT_PID=""

pass=0

# Delivery is the hard assertion: the agent logs a successful 2xx send. The
# journald service name derives from SYSLOG_IDENTIFIER, so it is the tag.
if grep -q "log batch sent" "$WORK/agent.log"; then
	echo "  ok    journald logs delivered (intake returned 2xx)"
else
	echo "  FAIL  journald logs not delivered"
	pass=1
fi

# Searchability is best-effort: whether ingested logs are queryable depends on the
# org's log indexes, so note but don't fail (same caveat as e2e.sh).
logs_searchable() {
	pup logs search --query="service:${TAG} ${TOKEN}" --from=20m --limit=10 --output json 2>/dev/null \
		| jq -e '.data | length > 0' >/dev/null
}
found=0
for _ in $(seq 1 12); do
	if logs_searchable; then found=1; break; fi
	sleep 10
done
if [ "$found" -eq 1 ]; then
	echo "  ok    journald log searchable via pup (service:${TAG})"
else
	echo "  note  journald log accepted but not searchable in this org's indexes (delivery confirmed above)"
fi

if [ "$pass" -eq 0 ]; then
	echo "==> JOURNALD E2E PASS"
else
	echo "==> JOURNALD E2E FAIL. Agent log tail:"
	tail -25 "$WORK/agent.log"
fi
exit "$pass"
