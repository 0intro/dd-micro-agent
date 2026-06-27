#!/usr/bin/env bash
#
# Side-by-side parity test: boot a Fedora VM, install the stock Agent
# (dnf), and run it next to our micro-agent. Both are pointed at our own local
# fake intake (e2e/parity), fed byte-identical DogStatsD input (one dsdsample
# fanned to both ports) and the same tailed log file, then their recorded
# payloads are diffed. Proves that, on the surface we implement, we send the
# stock Agent the same thing.
#
# Fully local: NO Datadog API keys and no Datadog network. Only the one-time
# `dnf install datadog-agent` needs outbound network (run with the sandbox off).
#
#   FEDORA_VER         Fedora release (default 44)
#   DD_AGENT_VERSION   pin the stock agent (e.g. 1:7.66.1-1), default = latest
#   STOP_AFTER         debug knob: boot | provision | feed | verify (default)
#
# The Fedora image + SSH key under /tmp/fedora-img and /tmp/fedora-vm are
# downloaded/generated if absent and reused on later runs (see e2e/vm_linux.sh).
#
set -uo pipefail

STOP_AFTER="${STOP_AFTER:-verify}"
FEDORA_VER="${FEDORA_VER:-44}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMG_DIR=/tmp/fedora-img
BASE_IMG="$IMG_DIR/fedora${FEDORA_VER}.qcow2"
KEY_DIR=/tmp/fedora-vm
SSH_KEY="$KEY_DIR/id_ed25519"
# Fedora Cloud Base qcow2. The spin suffix changes per release, so allow an override.
FEDORA_IMG_URL="${FEDORA_IMG_URL:-https://download.fedoraproject.org/pub/fedora/linux/releases/${FEDORA_VER}/Cloud/x86_64/images/Fedora-Cloud-Base-Generic-${FEDORA_VER}-1.7.x86_64.qcow2}"
W=/tmp/microagent-parity
SSH_PORT=2225

# Guest layout: our agent + fake intake run as the fedora user. The stock agent
# runs as dd-agent under systemd. The log both tail is world-readable.
GDIR=/home/fedora/parity
LOG=/var/log/parity/app.log
# A unique journald identifier both agents filter on, so the comparison sees one
# deterministic journald entry next to the file-tailed lines.
JTAG="microagent_parity_journald_$$"

SSH_COMMON="-i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o ServerAliveInterval=15 -o ServerAliveCountMax=4 -o LogLevel=ERROR"
vmssh() { ssh $SSH_COMMON -p "$SSH_PORT" fedora@127.0.0.1 "$@"; }
vmscp() { scp $SSH_COMMON -P "$SSH_PORT" "$1" fedora@127.0.0.1:"$2"; }

log() { echo "==> $*"; }
QEMU_PID=""
cleanup() {
	if [ -n "${KEEP_VM:-}" ]; then
		log "KEEP_VM set, leaving the guest up: ssh -i $SSH_KEY -p $SSH_PORT fedora@127.0.0.1"
		return
	fi
	log "cleanup"
	[ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null
	pkill -f "$W/overlay.qcow2" 2>/dev/null
	rm -rf "$W"
}
trap cleanup EXIT
stop_here() { if [ "$STOP_AFTER" = "$1" ]; then log "STOP_AFTER=$1 reached"; exit 0; fi; }

command -v qemu-system-x86_64 >/dev/null || { echo "qemu missing"; exit 1; }
pkill -f "hostfwd=tcp::${SSH_PORT}-:22" 2>/dev/null && sleep 1
rm -rf "$W"; mkdir -p "$W" "$IMG_DIR" "$KEY_DIR"
# SSH key + Fedora image: provisioned once, cached, reused locally and in CI.
[ -r "$SSH_KEY" ] || ssh-keygen -t ed25519 -N "" -f "$SSH_KEY" -q
PUBKEY="$(cat "$SSH_KEY.pub")"
if [ ! -r "$BASE_IMG" ]; then
	log "downloading Fedora ${FEDORA_VER} cloud image"
	curl -fL --no-progress-meter --connect-timeout 15 -o "$BASE_IMG" "$FEDORA_IMG_URL" ||
		{ echo "fedora image download failed (override FEDORA_IMG_URL)"; rm -f "$BASE_IMG"; exit 1; }
fi

log "building micro-agent + dsdsample + parity tool (linux/amd64)"
for pkg in cmd/agent e2e/dsdsample e2e/parity; do
	out="$W/$(basename "$pkg")"
	( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 go build -tags netgo -o "$out" "./$pkg" ) ||
		{ echo "build $pkg failed"; exit 1; }
done

log "building cloud-init seed + booting Fedora ${FEDORA_VER}"
cat > "$W/user-data" <<EOF
#cloud-config
ssh_pwauth: false
users:
  - name: fedora
    sudo: ALL=(ALL) NOPASSWD:ALL
    groups: [wheel]
    shell: /bin/bash
    ssh_authorized_keys:
      - ${PUBKEY}
EOF
printf 'instance-id: parity\nlocal-hostname: parity-host\n' > "$W/meta-data"
xorriso -as mkisofs -V cidata -J -r -o "$W/seed.iso" \
	-graft-points "user-data=$W/user-data" "meta-data=$W/meta-data" 2>"$W/xorriso.log" ||
	{ echo "seed build failed"; cat "$W/xorriso.log"; exit 1; }

qemu-img create -f qcow2 -b "$BASE_IMG" -F qcow2 "$W/overlay.qcow2" >/dev/null 2>&1
qemu-system-x86_64 -enable-kvm -cpu host -m 4096 -smp 2 \
	-drive file="$W/overlay.qcow2",format=qcow2,if=virtio \
	-drive file="$W/seed.iso",media=cdrom \
	-netdev user,id=net0,hostfwd=tcp::${SSH_PORT}-:22 -device virtio-net-pci,netdev=net0 \
	-display none -serial file:"$W/console.log" </dev/null >"$W/qemu.out" 2>&1 &
QEMU_PID=$!

log "waiting for SSH"
ssh_up=0
for _ in $(seq 1 60); do
	if vmssh true 2>/dev/null; then ssh_up=1; break; fi
	sleep 3
done
[ "$ssh_up" = 1 ] || { echo "SSH never came up; console tail:"; tail -30 "$W/console.log" 2>/dev/null; exit 1; }
log "VM up: $(vmssh 'cat /etc/fedora-release')"
stop_here boot

log "installing the stock Agent (dnf)"
vmssh "sudo setenforce 0 2>/dev/null; sudo tee /etc/yum.repos.d/datadog.repo >/dev/null" <<'EOF'
[datadog]
name=Datadog, Inc.
baseurl=https://yum.datadoghq.com/stable/7/x86_64/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://keys.datadoghq.com/DATADOG_RPM_KEY_CURRENT.public
       https://keys.datadoghq.com/DATADOG_RPM_KEY_B01082D3.public
       https://keys.datadoghq.com/DATADOG_RPM_KEY_FD4BF915.public
EOF
pkg="datadog-agent"; [ -n "${DD_AGENT_VERSION:-}" ] && pkg="datadog-agent-${DD_AGENT_VERSION}"
vmssh "sudo dnf install -y $pkg >/tmp/dd-install.log 2>&1" ||
	{ echo "stock agent install failed"; vmssh "tail -20 /tmp/dd-install.log"; exit 1; }
log "stock agent: $(vmssh '/opt/datadog-agent/bin/agent/agent version 2>/dev/null | head -1')"

log "staging our agent, dsdsample, parity tool + the shared log"
vmssh "mkdir -p $GDIR/ours/conf.d/parity.d $GDIR/rec && sudo mkdir -p $(dirname $LOG) && sudo touch $LOG && sudo chmod 0644 $LOG && sudo chmod 0755 $(dirname $LOG)"
vmscp "$W/agent" "$GDIR/agent";       vmssh "chmod 0755 $GDIR/agent"
vmscp "$W/dsdsample" "$GDIR/dsdsample"; vmssh "chmod 0755 $GDIR/dsdsample"
vmscp "$W/parity" "$GDIR/parity";     vmssh "chmod 0755 $GDIR/parity"

# Our agent -> fake :18080 (dogstatsd :8125).
vmssh "cat > $GDIR/ours/datadog.yaml" <<EOF
api_key: dummy
dd_url: http://127.0.0.1:18080
dogstatsd_port: 8125
hostname: parity-host
tags: [test:parity]
logs_enabled: true
enable_metadata_collection: true
run_path: ${GDIR}/ours
confd_path: ${GDIR}/ours/conf.d
logs_config: {logs_dd_url: http://127.0.0.1:18080}
process_config:
  process_dd_url: http://127.0.0.1:18080
  process_collection:
    enabled: true
apm_config:
  enabled: true
  receiver_port: 8136
  profiling_dd_url: http://127.0.0.1:18080/api/v2/profile
EOF
vmssh "cat > $GDIR/ours/conf.d/parity.d/conf.yaml" <<EOF
logs:
  - {type: file, path: ${LOG}, service: parity, source: parity}
  - type: journald
    service: parity
    source: parity
    start_position: beginning
    include_matches: ["SYSLOG_IDENTIFIER=${JTAG}"]
EOF

# Stock agent -> fake :18081 (dogstatsd :8126), forced into v1 JSON + plaintext HTTP.
vmssh "sudo tee /etc/datadog-agent/datadog.yaml >/dev/null" <<EOF
api_key: dummy
dd_url: http://127.0.0.1:18081
hostname: parity-host
tags:
  - test:parity
dogstatsd_port: 8126
# Force formats our stdlib fake can decode: the stock agent defaults to zstd for
# every serializer payload (series/metadata/inventory), which we can't read.
serializer_compressor_kind: gzip
use_v2_api:
  series: false
telemetry:
  enabled: false
apm_config:
  enabled: true
  receiver_port: 8137
  profiling_dd_url: http://127.0.0.1:18081/api/v2/profile
enable_metadata_collection: true
inventories_enabled: true
inventories_first_run_delay: 0
inventories_min_interval: 1
process_config:
  process_dd_url: http://127.0.0.1:18081
  process_collection:
    enabled: true
logs_enabled: true
logs_config:
  logs_dd_url: 127.0.0.1:18081
  force_use_http: true
  logs_no_ssl: true
  use_compression: false
EOF
vmssh "sudo mkdir -p /etc/datadog-agent/conf.d/parity.d && sudo tee /etc/datadog-agent/conf.d/parity.d/conf.yaml >/dev/null" <<EOF
logs:
  - {type: file, path: ${LOG}, service: parity, source: parity}
  - type: journald
    service: parity
    source: parity
    start_position: beginning
    include_matches: ["SYSLOG_IDENTIFIER=${JTAG}"]
EOF
vmssh "sudo chown -R dd-agent: /etc/datadog-agent"
# Both agents must read the journal. Our agent execs journalctl as fedora, which
# reads fedora's own logger entry without a group. The stock agent reads it via
# sdjournal as dd-agent, so put dd-agent in systemd-journal (the RPM usually does,
# this is defensive and idempotent).
vmssh "sudo usermod -aG systemd-journal dd-agent 2>/dev/null; true"
stop_here provision

# run. Start the fake intake and our agent with `setsid --fork`, not `nohup &`. A
# Go daemon backgrounded with `&` over ssh leaves the remote shell waiting on it, so
# the ssh channel never closes and the call wedges (a plain `sleep` does not trigger
# this, a multi-threaded Go binary does). setsid --fork reparents the daemon to init
# in its own session, so the shell returns at once and ssh comes back.
log "starting fake intake + both agents (ours + fake intake: setsid; stock: systemd core + process agent)"
vmssh "cd $GDIR && setsid --fork ./parity serve -dir $GDIR/rec ours=127.0.0.1:18080 stock=127.0.0.1:18081 >$GDIR/serve.log 2>&1 </dev/null; sleep 1; echo serving"
vmssh "cd $GDIR && setsid --fork ./agent --cfgpath ours/datadog.yaml --debug >ours/agent.log 2>&1 </dev/null; sleep 1; echo ours-started"

# The stock unit is Type=notify, so a plain `systemctl restart` blocks until the agent
# signals ready. Under hosted nested KVM that startup saturates the guest and the call
# wedges, so start it non-blocking and poll is-active with a bound, logging guest memory
# each tick (a climb toward total = OOM).
# The trace-agent (needed only for the profiling proxy) is started after the metrics
# feed, so its load does not make the stock DogStatsD drop UDP packets mid-workload.
vmssh "sudo systemctl restart --no-block datadog-agent; sudo systemctl restart --no-block datadog-agent-process 2>/dev/null; true"
core=inactive; unreach=0
for _ in $(seq 1 36); do # ~3 min bound
	core="$(vmssh 'systemctl is-active datadog-agent' 2>/dev/null || echo unreachable)"
	log "stock core: $core  $(vmssh 'free -m | grep Mem' 2>/dev/null || echo 'mem unreachable')"
	[ "$core" = active ] && break
	if [ "$core" = unreachable ]; then unreach=$((unreach + 1)); [ "$unreach" -ge 3 ] && break; else unreach=0; fi
	sleep 5
done
if [ "$core" != active ]; then
	echo "==> stock agent never became active under hosted nested KVM; diagnostics follow"
	echo "--- systemctl status ---"; vmssh "systemctl status datadog-agent --no-pager -l 2>&1 | head -40"
	echo "--- journalctl ---";       vmssh "sudo journalctl -u datadog-agent --no-pager -n 60 2>&1"
	echo "--- agent.log tail ---";   vmssh "sudo tail -40 /var/log/datadog/agent.log 2>&1"
	echo "--- OOM / dmesg ---";      vmssh "sudo dmesg 2>&1 | grep -iE 'oom|killed process|out of memory' | tail -20"
	echo "--- memory + load ---";    vmssh "free -m; uptime; nproc"
	exit 1
fi

# The stock agent reports "active" before its DogStatsD listener, host tagger, and
# log tailer have finished coming up. Feeding too soon loses metrics (connection
# refused on its 8126), ships them without the host tags (the tagger is not ready),
# and misses the log lines. Wait for the stock DogStatsD socket, then settle so the
# tagger and tailer are ready before driving identical input.
log "waiting for stock DogStatsD (8126) to bind, then settling"
vmssh "for _ in \$(seq 1 30); do ss -uln 2>/dev/null | grep -q ':8126 ' && break; sleep 2; done; ss -uln 2>/dev/null | grep ':8126 ' || echo 'WARNING: stock dogstatsd not listening'"
sleep 20

log "feeding identical input (dsdsample fan-out + shared log + one tagged journald line)"
vmssh "printf 'parity line one\nparity line two\nparity line three\n' | sudo tee -a $LOG >/dev/null"
# One uniquely tagged journal entry both agents follow and ship. Both read the same
# stored fields, so the structured body must come out byte-identical.
vmssh "logger -t $JTAG 'parity journald line $JTAG'"
vmssh "$GDIR/dsdsample -addr 127.0.0.1:8125,127.0.0.1:8126 -duration 25s -prefix microagent.parity"

# Now bring up the stock trace-agent for the profiling leg (ours is already up).
log "starting the stock trace-agent for the profiling proxy"
vmssh "sudo systemctl restart --no-block datadog-agent-trace 2>/dev/null; true"

# Profiling parity: feed an identical profiling upload to each proxy (ours:8136,
# stock:8137), the same controlled-input principle as the DogStatsD fan-out above. A
# profiler's multipart is opaque to the proxy (it forwards the bytes untouched), so a
# synthetic upload exercises the proxy exactly while keeping the input byte-identical
# on both sides. The compare then diffs only the agent's own contribution: the Via,
# X-Datadog-Additional-Tags, and DD-API-KEY headers each proxy injects. The real
# dd-trace-go and ddprof profilers are exercised by the live e2e (e2e/profiling.sh),
# not here, so this stays fast and free of the tracer's in-guest scheduling.
log "waiting for both profiling proxies to listen (ours:8136, stock:8137)"
vmssh "for _ in \$(seq 1 30); do ss -tln 2>/dev/null | grep -q ':8136 ' && ss -tln 2>/dev/null | grep -q ':8137 ' && break; sleep 2; done; ss -tln 2>/dev/null | grep -E ':8136 |:8137 ' || echo 'WARNING: a proxy port is not listening'"
log "feeding an identical profiling upload to both proxies"
vmssh "printf '{\"version\":\"4\",\"family\":\"go\",\"attachments\":[\"cpu.pprof\"],\"tags_profiler\":\"service:parity-prof,runtime:go\"}' >/tmp/ev.json;
	printf 'synthetic pprof bytes' >/tmp/cpu.pprof;
	for port in 8136 8137; do
		curl -sS -o /dev/null -w \"profiling proxy \$port -> HTTP %{http_code}\n\" \
			-F 'event=@/tmp/ev.json;filename=event.json;type=application/json' \
			-F 'cpu.pprof=@/tmp/cpu.pprof;filename=cpu.pprof' \
			http://127.0.0.1:\$port/profiling/v1/input;
	done"
sleep 20 # let both agents flush metrics + emit another process collect after the workload
stop_here feed

log "stopping both agents (final flush)"
vmssh "pkill -TERM -f 'agent --cfgpath ours/datadog.yaml'; sudo systemctl stop datadog-agent datadog-agent-process 2>/dev/null; sleep 3; pkill -INT -f 'parity serve'; sleep 1; true"
log "records: ours=$(vmssh "wc -l <$GDIR/rec/ours.jsonl 2>/dev/null") stock=$(vmssh "wc -l <$GDIR/rec/stock.jsonl 2>/dev/null")"

log "comparing recordings"
vmssh "$GDIR/parity compare $GDIR/rec/ours.jsonl $GDIR/rec/stock.jsonl"
rc=$?
if [ "$rc" -ne 0 ]; then
	echo "==> PARITY TEST FAILED (rc=$rc)"
	echo "--- stock agent log tail ---"; vmssh "sudo tail -15 /var/log/datadog/agent.log 2>/dev/null"
	echo "--- our agent log tail ---"; vmssh "tail -15 $GDIR/ours/agent.log 2>/dev/null"
fi
exit "$rc"
