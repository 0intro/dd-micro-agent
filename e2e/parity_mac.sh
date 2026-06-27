#!/usr/bin/env bash
#
# Side-by-side parity on macOS: run the stock Datadog Agent (installed from the
# official DMG) next to our micro-agent, natively on the host (no VM, since macOS
# cannot be virtualized off Apple hardware). Both are pointed at our local fake
# intake (e2e/parity), fed byte-identical DogStatsD input (one dsdsample fanned to
# both ports) plus the same tailed log file, then their recordings are diffed.
#
# Reduced surface vs the Linux parity (e2e/parity.sh): the stock macOS Agent ships
# no process-agent, so Live Processes is not compared, and it emits cpu/mem host
# metrics our cgo-free darwin agent doesn't (we stay an accepted subset). So the
# compare runs with -platform darwin, which skips only the Live Processes tier.
#
# Fully local and keyless: no Datadog API key, no pup, no Datadog network. Only the
# one-time DMG download needs the internet. On CI this runs on a GitHub macos-15
# runner (real Apple hardware).
#
#   STOP_AFTER   debug knob: install | provision | feed | verify (default)
#
set -uo pipefail

STOP_AFTER="${STOP_AFTER:-verify}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
W="$(mktemp -d 2>/dev/null || mktemp -d -t microagent-parity-mac)"

OURS_INTAKE=18080
STOCK_INTAKE=18081
OURS_DSD=8125
STOCK_DSD=8126
# A world-readable shared log both agents tail (the stock daemon may run as a
# different user than the runner, so keep it out of the runner-owned mktemp dir).
LOG=/tmp/parity_mac_app.log

# Stock macOS Agent layout (installer -pkg drops it here).
DD_DIR=/opt/datadog-agent
DD_BIN="$DD_DIR/bin/agent/agent"
DD_CONF="$DD_DIR/etc/datadog.yaml"
DD_CONFD="$DD_DIR/etc/conf.d"

log() { echo "==> $*"; }
OURS_PID=""; SERVE_PID=""

# The pkg registers a KeepAlive launchd daemon on the default DogStatsD 8125; a plain kill
# is restarted, so bootout (stop) + disable (stay down) it, then pkill any lingering run.
stock_stop() {
	sudo launchctl bootout system/com.datadoghq.agent 2>/dev/null
	sudo launchctl disable system/com.datadoghq.agent 2>/dev/null
	sudo pkill -TERM -f "$DD_DIR/bin/agent/agent run" 2>/dev/null
	true
}
cleanup() {
	log "cleanup"
	[ -n "$OURS_PID" ] && kill -INT "$OURS_PID" 2>/dev/null
	stock_stop
	[ -n "$SERVE_PID" ] && kill -INT "$SERVE_PID" 2>/dev/null
	rm -rf "$W" "$LOG"
}
trap cleanup EXIT
stop_here() { if [ "$STOP_AFTER" = "$1" ]; then log "STOP_AFTER=$1 reached"; exit 0; fi; }

command -v go >/dev/null || { echo "go missing"; exit 1; }
log "host: $(uname -sm)  run dir: $W"

log "building agent + dsdsample + parity (native)"
gobuild() { GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 go -C "$ROOT" build -tags netgo -o "$1" "$2"; }
gobuild "$W/agent" ./cmd/agent        || { echo "agent build failed"; exit 1; }
gobuild "$W/dsdsample" ./e2e/dsdsample || { echo "dsdsample build failed"; exit 1; }
gobuild "$W/parity" ./e2e/parity      || { echo "parity build failed"; exit 1; }

# Install the stock Agent from the official DMG, headless: mount, installer -pkg,
# detach. It lands under /opt/datadog-agent and registers a system launchd daemon.
# No API key is needed to install (we drive the binary directly with our own config).
if [ ! -x "$DD_BIN" ]; then
	log "installing the stock Datadog Agent (DMG, headless)"
	case "$(uname -m)" in
		arm64) DMG=datadog-agent-7-latest.arm64.dmg ;;
		*)     DMG=datadog-agent-7-latest.dmg ;;
	esac
	curl -fL --no-progress-meter -o "$W/dd.dmg" "https://install.datadoghq.com/$DMG" ||
		{ echo "DMG download failed"; exit 1; }
	sudo hdiutil detach /Volumes/datadog_agent >/dev/null 2>&1 || true
	sudo hdiutil attach "$W/dd.dmg" -mountpoint /Volumes/datadog_agent -nobrowse >/dev/null ||
		{ echo "DMG attach failed"; exit 1; }
	PKG="$(find /Volumes/datadog_agent -name '*.pkg' 2>/dev/null | head -1)"
	[ -n "$PKG" ] || { echo "no .pkg in the DMG"; sudo hdiutil detach /Volumes/datadog_agent >/dev/null 2>&1; exit 1; }
	sudo installer -pkg "$PKG" -target / || { echo "pkg install failed"; sudo hdiutil detach /Volumes/datadog_agent >/dev/null 2>&1; exit 1; }
	sudo hdiutil detach /Volumes/datadog_agent >/dev/null 2>&1 || true
fi
[ -x "$DD_BIN" ] || { echo "stock agent binary not found at $DD_BIN after install"; exit 1; }
log "stock agent version: $(sudo "$DD_BIN" version 2>/dev/null | head -1)"
# The pkg's postinstall may have started the daemon (keyless); stop it before we reconfigure.
stock_stop; sleep 2
stop_here install

# provision: our agent -> fake :18080 (dogstatsd 8125), stock -> fake :18081 (8126),
# both tailing the one shared log. Same hostname so the metadata compares.
HOSTN=parity-mac-host
: > "$LOG"; chmod 0644 "$LOG"
mkdir -p "$W/ours/conf.d/parity.d" "$W/rec"

cat > "$W/ours/datadog.yaml" <<EOF
api_key: dummy
dd_url: http://127.0.0.1:${OURS_INTAKE}
dogstatsd_port: ${OURS_DSD}
hostname: ${HOSTN}
tags: [test:parity]
logs_enabled: true
enable_metadata_collection: true
run_path: ${W}/ours
confd_path: ${W}/ours/conf.d
logs_config: {logs_dd_url: http://127.0.0.1:${OURS_INTAKE}}
process_config:
  process_dd_url: http://127.0.0.1:${OURS_INTAKE}
  process_collection:
    enabled: true
EOF
cat > "$W/ours/conf.d/parity.d/conf.yaml" <<EOF
logs:
  - {type: file, path: ${LOG}, service: parity, source: parity}
EOF

# Stock config: force the formats our stdlib fake intake decodes (gzip not zstd, v1
# JSON series, plaintext HTTP logs). No process-agent on macOS, so leave it off.
sudo tee "$DD_CONF" >/dev/null <<EOF
api_key: dummy
dd_url: http://127.0.0.1:${STOCK_INTAKE}
dogstatsd_port: ${STOCK_DSD}
hostname: ${HOSTN}
tags:
  - test:parity
serializer_compressor_kind: gzip
use_v2_api:
  series: false
telemetry:
  enabled: false
enable_metadata_collection: true
inventories_enabled: true
inventories_first_run_delay: 0
inventories_min_interval: 1
apm_config:
  enabled: false
process_config:
  process_collection:
    enabled: false
logs_enabled: true
logs_config:
  logs_dd_url: 127.0.0.1:${STOCK_INTAKE}
  force_use_http: true
  logs_no_ssl: true
  use_compression: false
EOF
sudo mkdir -p "$DD_CONFD/parity.d"
sudo tee "$DD_CONFD/parity.d/conf.yaml" >/dev/null <<EOF
logs:
  - {type: file, path: ${LOG}, service: parity, source: parity}
EOF

# run: fake intake first (captures startup metadata), then both agents. Bootout the pkg's
# launchd daemon again (belt and suspenders) and confirm DogStatsD 8125 is free before our
# agent binds it, so the two never race for the port.
log "ensuring the stock launchd daemon is down (frees DogStatsD ${OURS_DSD})"
stock_stop
# sudo lsof so it sees the root daemon's socket: a non-root lsof reports the port free when
# it is not, letting our agent race the daemon's lingering bind ("address already in use").
for _ in $(seq 1 30); do sudo lsof -nP -iUDP:${OURS_DSD} >/dev/null 2>&1 || break; sleep 1; done
sudo lsof -nP -iUDP:${OURS_DSD} >/dev/null 2>&1 && echo "WARNING: UDP ${OURS_DSD} still held: $(sudo lsof -nP -iUDP:${OURS_DSD} 2>/dev/null | tail -2)"
log "starting fake intake + both agents"
"$W/parity" serve -dir "$W/rec" ours=127.0.0.1:${OURS_INTAKE} stock=127.0.0.1:${STOCK_INTAKE} >"$W/serve.log" 2>&1 &
SERVE_PID=$!
sleep 1
"$W/agent" --cfgpath "$W/ours/datadog.yaml" --debug >"$W/ours.log" 2>&1 &
OURS_PID=$!
sudo "$DD_BIN" run -c "$DD_CONF" >"$W/stock.log" 2>&1 &
sleep 3
log "ours: $(pgrep -f "$W/agent" | head -1)  stock: $(pgrep -f "$DD_DIR/bin/agent/agent run" | head -1)"

# Wait for the stock DogStatsD socket to bind before feeding, then settle so its
# tagger and log tailer are ready (feeding too soon drops metrics/tags/logs).
log "waiting for stock DogStatsD (${STOCK_DSD}) to bind"
for _ in $(seq 1 30); do
	if sudo lsof -nP -iUDP:${STOCK_DSD} >/dev/null 2>&1; then break; fi
	sleep 2
done
sudo lsof -nP -iUDP:${STOCK_DSD} >/dev/null 2>&1 || echo "WARNING: stock dogstatsd not listening on ${STOCK_DSD}"
sleep 15
stop_here provision

log "feeding identical input (dsdsample fan-out + shared log)"
printf 'parity line one\nparity line two\nparity line three\n' >> "$LOG"
"$W/dsdsample" -addr 127.0.0.1:${OURS_DSD},127.0.0.1:${STOCK_DSD} -duration 25s -prefix microagent.parity
sleep 20 # let both flush metrics + emit metadata
stop_here feed

log "stopping both agents (final flush)"
kill -INT "$OURS_PID" 2>/dev/null; OURS_PID=""
stock_stop
sleep 3
kill -INT "$SERVE_PID" 2>/dev/null; SERVE_PID=""
sleep 1
log "records: ours=$(wc -l <"$W/rec/ours.jsonl" 2>/dev/null) stock=$(wc -l <"$W/rec/stock.jsonl" 2>/dev/null)"

log "comparing recordings (parity compare -platform darwin)"
if "$W/parity" compare -platform darwin "$W/rec/ours.jsonl" "$W/rec/stock.jsonl"; then
	echo "==> MAC PARITY PASS"; exit 0
fi
echo "==> MAC PARITY FAIL"
echo "--- record kinds: ours ---";  sed -E 's/^\{"kind":"([a-z_]+)".*/\1/' "$W/rec/ours.jsonl"  2>/dev/null | sort | uniq -c
echo "--- record kinds: stock ---"; sed -E 's/^\{"kind":"([a-z_]+)".*/\1/' "$W/rec/stock.jsonl" 2>/dev/null | sort | uniq -c
echo "--- stock agent log tail ---"; tail -25 "$W/stock.log" 2>/dev/null
echo "--- our agent log tail ---";   tail -15 "$W/ours.log" 2>/dev/null
exit 1
