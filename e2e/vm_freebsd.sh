#!/usr/bin/env bash
#
# Full end-to-end test on FreeBSD: boot a FreeBSD cloud VM, run the cross-built
# micro-agent (metrics + logs + host metadata enabled) as the unprivileged
# `freebsd` user, run a DogStatsD sample service plus an nginx-format access log, and
# verify via the `pup` CLI that the DogStatsD metrics, the FreeBSD host metrics (mem/load
# via sysctl), the logs, and the host itself (with its freebsd platform/gohai)
# all reached Datadog.
#
# This is the live proof for the BSD host-stats path (kern.cp_time, vm.loadavg,
# hw.physmem/vm.stats, getfsstat, kern.boottime), all of which read sysctl and
# need no privileges.
#
# Why no root / no real nginx: the official FreeBSD cloud image is provisioned by
# FreeBSD base's `nuageinit`, not the Python cloud-init. nuageinit authorizes SSH
# keys but ignores `packages:`/`runcmd:`/`write_files:`, so we cannot install
# sudo/nginx or get a root shell. Everything the agent needs (sysctl, getfsstat,
# tailing a file, UDP 8125, HTTPS out) works unprivileged, so we run as `freebsd`
# and synthesize the access log that nginx would have written.
#
# Two modes. With DD_API_KEY + DD_APP_KEY set it posts to real Datadog and verifies
# with the `pup` CLI (the manual mode). With DD_API_KEY unset it posts to a local
# fake intake (e2e/parity) running in the guest and verifies the recording with
# `parity verify` (the automated CI mode: no keys, no pup, no network to Datadog).
#
#   DD_API_KEY / DD_APP_KEY   set for the real-Datadog + pup mode, unset for fake intake
#   DD_SITE                   defaults to datadoghq.com (real mode only)
#   FREEBSD_IMG_URL           override the cloud image URL
#   STOP_AFTER                debug knob: boot | provision | traffic | verify (default)
#
# Needs KVM + outbound network (run with the sandbox off). Downloads and caches
# the FreeBSD cloud image and an SSH key under /tmp/freebsd-img and /tmp/freebsd-vm.
#
set -uo pipefail

# With DD_API_KEY set: real Datadog + pup. Without it: the in-guest fake intake.
FAKE=0
[ -z "${DD_API_KEY:-}" ] && FAKE=1
if [ "$FAKE" = 0 ]; then
	: "${DD_APP_KEY:?set DD_APP_KEY (or unset DD_API_KEY to use the fake-intake mode)}"
	export DD_API_KEY DD_APP_KEY
fi
export DD_SITE="${DD_SITE:-datadoghq.com}"
STOP_AFTER="${STOP_AFTER:-verify}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMG_VER=15.1-RELEASE
IMG_NAME="FreeBSD-${IMG_VER}-amd64-BASIC-CLOUDINIT-ufs"
IMG_DIR=/tmp/freebsd-img
BASE_IMG="$IMG_DIR/${IMG_NAME}.qcow2"
IMG_URL="${FREEBSD_IMG_URL:-https://download.freebsd.org/releases/VM-IMAGES/${IMG_VER}/amd64/Latest/${IMG_NAME}.qcow2.xz}"
KEY_DIR=/tmp/freebsd-vm
SSH_KEY="$KEY_DIR/id_ed25519"
W=/tmp/microagent-vm-freebsd
SSH_PORT=2223
GUEST_DIR=/home/freebsd/dd-e2e

RUN_ID="$(date +%s)$RANDOM"
HOST_NAME="microagent-fbsd-e2e-${RUN_ID}"
RUN_TAG="e2e_run:${RUN_ID}"
TEST_TAG="test:microagent-e2e"
QEMU_PID=""

SSH_COMMON="-i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o LogLevel=ERROR"
vmssh() { ssh $SSH_COMMON -p "$SSH_PORT" freebsd@127.0.0.1 "$@"; }
vmscp() { scp $SSH_COMMON -P "$SSH_PORT" "$1" freebsd@127.0.0.1:"$2"; }

# ddpup runs pup with an isolated HOME+config so it authenticates with the
# provided keys (not a stored OAuth session) and reads the same org the agent
# writes to.
PUP_HOME="$W/pup"
ddpup() { HOME="$PUP_HOME" XDG_CONFIG_HOME="$PUP_HOME" pup "$@"; }

log() { echo "==> $*"; }

cleanup() {
	log "cleanup"
	[ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null
	pkill -f "$W/overlay.qcow2" 2>/dev/null
	rm -rf "$W"
}
trap cleanup EXIT

stop_here() { # $1 = stage just completed
	if [ "$STOP_AFTER" = "$1" ]; then log "STOP_AFTER=$1 reached"; exit 0; fi
}

command -v qemu-system-x86_64 >/dev/null || { echo "qemu missing"; exit 1; }
command -v xz >/dev/null || { echo "xz missing"; exit 1; }
pkill -f "hostfwd=tcp::${SSH_PORT}-:22" 2>/dev/null && sleep 1 # kill any stale VM on our port
rm -rf "$W"; mkdir -p "$PUP_HOME" "$IMG_DIR" "$KEY_DIR"

[ -r "$SSH_KEY" ] || ssh-keygen -t ed25519 -N "" -f "$SSH_KEY" -q
PUBKEY="$(cat "$SSH_KEY.pub")"

if [ ! -r "$BASE_IMG" ]; then
	log "downloading FreeBSD ${IMG_VER} cloud image"
	curl -fL --no-progress-meter --connect-timeout 15 -o "$BASE_IMG.xz" "$IMG_URL" || { echo "image download failed"; exit 1; }
	log "decompressing image"
	unxz -f "$BASE_IMG.xz" || { echo "image decompress failed"; exit 1; }
fi
log "run_id=$RUN_ID host=$HOST_NAME site=$DD_SITE"

log "building static agent + dogstatsd sample (freebsd/amd64)"
( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 \
	go build -tags netgo -o "$W/agent" ./cmd/agent ) ||
	{ echo "build failed"; exit 1; }
( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 \
	go build -tags netgo -o "$W/dsdsample" ./e2e/dsdsample ) ||
	{ echo "dsdsample build failed"; exit 1; }
if [ "$FAKE" = 1 ]; then
	( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 \
		go build -tags netgo -o "$W/parity" ./e2e/parity ) ||
		{ echo "parity build failed"; exit 1; }
fi

# nuageinit honors only users/ssh-keys/groups/hostname, so the seed just
# authorizes our key for the freebsd user. All setup happens over SSH afterwards.
log "building cloud-init seed"
cat > "$W/user-data" <<EOF
#cloud-config
ssh_pwauth: false
users:
  - name: freebsd
    groups: [wheel]
    shell: /bin/sh
    ssh_authorized_keys:
      - ${PUBKEY}
EOF
printf 'instance-id: %s\nlocal-hostname: %s\n' "$HOST_NAME" "$HOST_NAME" > "$W/meta-data"
xorriso -as mkisofs -V cidata -J -r -o "$W/seed.iso" \
	-graft-points "user-data=$W/user-data" "meta-data=$W/meta-data" 2>"$W/xorriso.log" ||
	{ echo "seed build failed"; cat "$W/xorriso.log"; exit 1; }

log "creating overlay + booting VM"
qemu-img create -f qcow2 -b "$BASE_IMG" -F qcow2 "$W/overlay.qcow2" >/dev/null 2>&1
qemu-system-x86_64 -enable-kvm -cpu host -m 4096 -smp 2 \
	-drive file="$W/overlay.qcow2",format=qcow2,if=virtio \
	-drive file="$W/seed.iso",media=cdrom \
	-netdev user,id=net0,hostfwd=tcp::${SSH_PORT}-:22 -device virtio-net-pci,netdev=net0 \
	-display none -serial file:"$W/console.log" </dev/null >"$W/qemu.out" 2>&1 &
QEMU_PID=$!

log "waiting for SSH"
ssh_up=0
for _ in $(seq 1 80); do
	if vmssh true 2>/dev/null; then ssh_up=1; break; fi
	sleep 3
done
[ "$ssh_up" = 1 ] || { echo "SSH never came up; console tail:"; tail -40 "$W/console.log" 2>/dev/null; exit 1; }
log "VM up: $(vmssh 'uname -sr')"

# Capture the kernel's devstat layout + a live sample + iostat ground truth, so the
# disk-I/O collector's struct offsets can be pinned/verified against this exact FreeBSD
# release (persisted outside $W, which cleanup wipes).
CAP=/tmp/fbsd-devstat-cap; mkdir -p "$CAP"
vmssh "cat /usr/include/sys/devicestat.h" > "$CAP/devicestat.h" 2>/dev/null
vmssh "sysctl -b kern.devstat.all" > "$CAP/devstat.bin" 2>/dev/null
vmssh "sysctl -b kern.devstat.all | hexdump -C | head -60" > "$CAP/devstat.hex" 2>/dev/null
vmssh "uname -mr; getconf LONG_BIT; sysctl kern.devstat.version kern.devstat.numdevs; echo '--- iostat -x ---'; iostat -x 1 2" > "$CAP/info.txt" 2>/dev/null
log "captured devstat layout/sample/iostat to $CAP ($(wc -c <"$CAP/devstat.bin" 2>/dev/null) bytes)"

# Same idea for the process collector: capture the kinfo_proc layout + a live blob so its
# amd64 struct offsets can be pinned/verified against this exact release. parseKinfoProcs
# decodes at offsets valid when sizeof(struct kinfo_proc) is 1088 (FreeBSD 12 to 15 amd64).
# The first record's ki_structsize (its leading int) is that size, so check it here.
vmssh "cat /usr/include/sys/user.h" > "$CAP/user.h" 2>/dev/null
vmssh "sysctl -b kern.proc.all" > "$CAP/proc-all.bin" 2>/dev/null
KP_SS=$(od -An -tu4 -N4 "$CAP/proc-all.bin" 2>/dev/null | tr -d ' ')
if [ "${KP_SS:-0}" = 1088 ]; then
	log "captured kinfo_proc layout to $CAP (ki_structsize=$KP_SS, as expected)"
else
	log "WARNING: kinfo_proc ki_structsize=${KP_SS:-?}, expected 1088. Process offsets may be off for this release"
fi
stop_here boot

log "installing micro-agent (unprivileged, under $GUEST_DIR)"
vmssh "mkdir -p $GUEST_DIR/conf.d/nginx.d $GUEST_DIR/run && : > $GUEST_DIR/access.log"
vmscp "$W/agent" "$GUEST_DIR/agent"
vmscp "$W/dsdsample" "$GUEST_DIR/dsdsample"
vmssh "chmod 0755 $GUEST_DIR/agent $GUEST_DIR/dsdsample"

if [ "$FAKE" = 1 ]; then
	vmscp "$W/parity" "$GUEST_DIR/parity"; vmssh "chmod 0755 $GUEST_DIR/parity"
	# Fake-intake config: post metrics, logs, metadata, and processes to the in-guest
	# recorder over plain HTTP. No api key, no real Datadog.
	vmssh "cat > $GUEST_DIR/datadog.yaml" <<EOF
api_key: dummy
dd_url: http://127.0.0.1:18080
hostname: ${HOST_NAME}
tags:
  - ${TEST_TAG}
  - ${RUN_TAG}
logs_enabled: true
enable_metadata_collection: true
run_path: ${GUEST_DIR}/run
confd_path: ${GUEST_DIR}/conf.d
logs_config: {logs_dd_url: http://127.0.0.1:18080}
process_config:
  process_dd_url: http://127.0.0.1:18080
  process_collection:
    enabled: true
EOF
else
	vmssh "cat > $GUEST_DIR/datadog.yaml" <<EOF
api_key: ${DD_API_KEY}
site: ${DD_SITE}
hostname: ${HOST_NAME}
tags:
  - ${TEST_TAG}
  - ${RUN_TAG}
logs_enabled: true
enable_metadata_collection: true
run_path: ${GUEST_DIR}/run
confd_path: ${GUEST_DIR}/conf.d
process_config:
  process_collection:
    enabled: true
EOF
fi

vmssh "cat > $GUEST_DIR/conf.d/nginx.d/conf.yaml" <<EOF
logs:
  - type: file
    path: ${GUEST_DIR}/access.log
    service: nginx
    source: nginx
EOF

# In fake mode, start the in-guest recorder before the agent so it captures the
# startup host metadata too. Start each daemon with "cd dir; nohup cmd ... &", not
# "cd dir && cmd &": backgrounding a "&&" list runs it in a subshell that waits for
# the daemon while holding the SSH channel open, which hangs the call. Backgrounding
# the simple redirected command (after a plain cd) lets the SSH session close at once.
if [ "$FAKE" = 1 ]; then
	vmssh "cd $GUEST_DIR; nohup ./parity serve -dir rec ours=127.0.0.1:18080 >parity.log 2>&1 </dev/null & sleep 1; echo serving"
fi
# Start the agent detached (no systemd/daemon needed for an unprivileged run).
vmssh "cd $GUEST_DIR; nohup ./agent --cfgpath datadog.yaml --debug >agent.log 2>&1 </dev/null & sleep 1; echo started"
sleep 3
log "agent process: $(vmssh "pgrep -lf 'agent --cfgpath' | tail -1")"
log "agent log (head):"; vmssh "head -12 $GUEST_DIR/agent.log"
stop_here provision

log "writing nginx-format access log"
vmssh "cd $GUEST_DIR; \
       for i in \$(seq 1 25); do printf '127.0.0.1 - - [%s] \"GET / HTTP/1.1\" 200 12 \"-\" \"e2e\"\n' \"\$(date '+%d/%b/%Y:%H:%M:%S %z')\" >> access.log; done; \
       printf '127.0.0.1 - - [%s] \"GET /microagent-e2e-${RUN_ID} HTTP/1.1\" 200 5 \"-\" \"e2e\"\n' \"\$(date '+%d/%b/%Y:%H:%M:%S %z')\" >> access.log"

# A sample 'service' emits a full DogStatsD workload (gauge, counter, histogram,
# timing, set, plus a service check and an event) over ~25s so it spans an agent
# flush. This exercises the whole metrics forwarding pipeline, not just a gauge.
log "running dogstatsd sample service (full metric workload)"
vmssh "$GUEST_DIR/dsdsample -addr 127.0.0.1:8125 -duration 25s -tags '${RUN_TAG},${TEST_TAG}' -prefix microagent.vm.dsd"
sleep 8
log "agent log (tail):"; vmssh "tail -12 $GUEST_DIR/agent.log"
stop_here traffic

# Fake-intake mode: stop the agent + recorder, then assert the recording carries the
# DogStatsD workload, the FreeBSD host metrics, host metadata (platform freebsd), the
# process list, and the unique log line. No pup, no real Datadog.
if [ "$FAKE" = 1 ]; then
	log "stopping agent + recorder (final flush)"
	vmssh "pkill -TERM -f 'agent --cfgpath'; sleep 3; pkill -INT -f 'parity serve'; sleep 1; true"
	log "records: $(vmssh "wc -l <$GUEST_DIR/rec/ours.jsonl 2>/dev/null")"
	log "verifying the fake-intake recording (parity verify)"
	if vmssh "$GUEST_DIR/parity verify \
		-series datadog.agent.running,microagent.vm.dsd.gauge,microagent.vm.dsd.requests,microagent.vm.dsd.render.95percentile,microagent.vm.dsd.latency.avg,microagent.vm.dsd.users,system.mem.total,system.disk.total,system.load.1,system.uptime,system.io.r_s \
		-check microagent.vm.dsd.check -event 'dsdsample up' \
		-platform freebsd -meta -host ${HOST_NAME} \
		-min-procs 10 -proc-name agent \
		-log microagent-e2e-${RUN_ID} \
		$GUEST_DIR/rec/ours.jsonl"; then
		echo "==> FREEBSD VM E2E (fake intake) PASS"; exit 0
	fi
	echo "==> FREEBSD VM E2E (fake intake) FAIL"
	echo "--- agent log tail ---"; vmssh "tail -20 $GUEST_DIR/agent.log 2>/dev/null"
	exit 1
fi

metric_is_42() {
	ddpup metrics query --query="avg:microagent.vm.dsd.gauge{${RUN_TAG}}" --from=15m --output json 2>/dev/null |
		jq -e '[.data.series[]?.pointlist[]?[1] | select(. != null)] | any(. == 42)' >/dev/null
}
metric_present() { # $1 = metric name
	ddpup metrics query --query="avg:$1{${RUN_TAG}}" --from=15m --output json 2>/dev/null |
		jq -e '.data.series | length > 0' >/dev/null
}
host_metric_present() { # proves the FreeBSD memory collector (hw.physmem/vm.stats)
	ddpup metrics query --query="avg:system.mem.total{${RUN_TAG}}" --from=15m --output json 2>/dev/null |
		jq -e '.data.series | length > 0' >/dev/null
}
load_metric_present() { # proves the FreeBSD load collector (vm.loadavg)
	ddpup metrics query --query="avg:system.load.1{${RUN_TAG}}" --from=15m --output json 2>/dev/null |
		jq -e '.data.series | length > 0' >/dev/null
}
io_metric_present() { # proves the FreeBSD disk-I/O collector (kern.devstat.all): the
	# series exists AND carries a device tag decoded from the binary devstat struct.
	# This VM's virtio disk is vtbd0 (its name+unit was reconstructed from the blob).
	ddpup metrics query --query="avg:system.io.r_s{${RUN_TAG}} by {device}" --from=15m --output json 2>/dev/null |
		jq -e '[.data.series[]?.scope // empty | select(test("device:vtbd"))] | length > 0' >/dev/null
}
host_present() {
	# The host appears in the Infrastructure List carrying our tags, the agent
	# version + gohai from the v5 metadata payload (agent_version is only set by
	# that payload, so it proves metadata, not just metrics, was ingested).
	ddpup infrastructure hosts list --filter="host:${HOST_NAME}" --output json 2>/dev/null |
		jq -e --arg t "$RUN_TAG" '.data.host_list[0]
			| (.tags_by_source.Datadog | index($t))
			and ((.meta.agent_version // "") != "")' >/dev/null
}
logs_searchable() {
	ddpup logs search --query="service:nginx ${RUN_TAG}" --from=20m --limit=10 --output json 2>/dev/null |
		jq -e '.data | length > 0' >/dev/null
}
wait_for() {
	local desc=$1 timeout=$2; shift 2
	local waited=0
	until "$@"; do
		waited=$((waited + 10))
		if [ "$waited" -gt "$timeout" ]; then echo "  FAIL  $desc (timeout ${waited}s)"; return 1; fi
		sleep 10
	done
	echo "  ok    $desc"
}

log "verifying via pup (ingestion latency ~minutes)"
pass=0
# DogStatsD pipeline: the gauge value is exact, the counter ships as a rate, the
# histogram expands to .avg/.95percentile/…, the set ships a distinct-member count.
wait_for "dogstatsd gauge = 42"                      300 metric_is_42                                         || pass=1
wait_for "dogstatsd counter present"                 300 metric_present microagent.vm.dsd.requests            || pass=1
wait_for "dogstatsd histogram .95percentile present" 300 metric_present microagent.vm.dsd.render.95percentile || pass=1
wait_for "dogstatsd set distinct-count present"      300 metric_present microagent.vm.dsd.users               || pass=1
wait_for "host metric system.mem.total present" 300 host_metric_present || pass=1
wait_for "host metric system.load.1 present" 300 load_metric_present    || pass=1
wait_for "disk I/O system.io.r_s{device:vtbd*} present" 300 io_metric_present || pass=1
# Host metadata: the hard assertion is that the agent *delivered* it (the v5 + inventory
# payloads, each a 2xx), the same split logs use (agent-2xx hard, pup best-effort). The
# Infrastructure List indexes a short-lived host with minutes of lag on some sites (EU
# prod exceeds 300s), so its appearance there is polled best-effort and never fails the run.
if vmssh "grep -q 'host metadata sent' $GUEST_DIR/agent.log"; then
	echo "  ok    host metadata delivered (agent 2xx)"
else
	echo "  FAIL  host metadata not delivered by agent"; pass=1
fi
hp=0; for _ in $(seq 1 18); do host_present && { hp=1; break; }; sleep 10; done
if [ "$hp" = 1 ]; then
	echo "  ok    host in Infrastructure List w/ tags+metadata"
else
	echo "  ..    host in Infrastructure List pending (indexing lag; metadata delivered above)"
fi

if vmssh "grep -q 'log batch sent' $GUEST_DIR/agent.log"; then
	echo "  ok    logs delivered (agent 2xx)"
else
	echo "  FAIL  logs not delivered by agent"; pass=1
fi
# Service checks and events: delivery is the assertion (the aggregator logs a 2xx),
# proving the _sc/_e parse plus the check_run and /intake/ event forwarding paths.
if vmssh "grep -q 'service checks sent' $GUEST_DIR/agent.log"; then echo "  ok    service check delivered (agent 2xx)"; else echo "  FAIL  service check not delivered"; pass=1; fi
if vmssh "grep -q 'events sent' $GUEST_DIR/agent.log"; then echo "  ok    event delivered (agent 2xx)"; else echo "  FAIL  event not delivered"; pass=1; fi
wait_for "logs searchable via pup" 300 logs_searchable || pass=1

# Live Processes (process intake)
# The agent posts the FULL FreeBSD process list (CollectorProc) to process.<site>,
# decoded from the kern.proc.all sysctl (struct kinfo_proc) by the freebsd collector.
# The 2xx is a hard gate (the Reporter logs the line only on success). The table then
# renders in Live Processes. A regression in the kinfo_proc offsets or process identity
# (createTime) collapses the list, which the assertions below catch.
if vmssh "grep -q 'process payload sent' $GUEST_DIR/agent.log"; then
	echo "  ok    process payload delivered (agent 2xx)"
else
	echo "  FAIL  process payload not delivered by agent"; pass=1
fi

PROC_JSON="$W/processes.json"
GT_PROCS=$(vmssh "ps -axo pid= | wc -l" 2>/dev/null | tr -d ' ')   # processes the VM's ps saw
vmssh "ps -axww -o pid,ppid,user,rss,nlwp,state,comm" > "$W/ps.txt" 2>/dev/null
# The v2 /processes endpoint returns a GLOBAL snapshot whose server-side host filter is
# unreliable, so we fetch the global list and filter to our host client-side. The host:
# filter itself is exercised through pup below (which does filter correctly).
fetch_procs() {
	curl -s -m 30 -H "DD-API-KEY: ${DD_API_KEY}" -H "DD-APPLICATION-KEY: ${DD_APP_KEY}" \
		"https://api.${DD_SITE}/api/v2/processes?page%5Blimit%5D=1000" >"$PROC_JSON" 2>/dev/null
}
host_proc_count() { fetch_procs; jq --arg h "$HOST_NAME" '[.data[]?|select(.attributes.host==$h)]|length' "$PROC_JSON" 2>/dev/null; }
# A real FreeBSD host surfaces its userland procs (init/sshd/syslogd/cron/agent/…). The
# >=10 floor catches an empty/collapsed table without being flaky about the exact count
# (zero-RSS kernel threads may not surface, as on the other platforms).
procs_present() { local n; n=$(host_proc_count); [ "${n:-0}" -ge 10 ]; }
has_cmd() { jq -e --arg h "$HOST_NAME" --arg c "$1" 'any(.data[]?; .attributes.host==$h and (.attributes.tags|index("command:"+$c)))' "$PROC_JSON" >/dev/null 2>&1; }
pup_proc_n()   { ddpup --no-agent processes list --tags "host:$1" --page-limit 1000 --output json 2>/dev/null | jq '[.data[]?]|length' 2>/dev/null; }
pup_all_match() { ddpup --no-agent processes list --tags "host:$HOST_NAME" --page-limit 1000 --output json 2>/dev/null | jq -e --arg h "$HOST_NAME" 'all(.data[]?; .attributes.host==$h)' >/dev/null 2>&1; }

log "verifying Live Processes (process intake) via the v2 processes API"
wait_for "process list present in Live Processes (>=10 procs)" 360 procs_present || pass=1
N=$(host_proc_count 2>/dev/null || echo 0)
echo "  ..    Live Processes shows ${N} of ${GT_PROCS:-?} sent"
# The agent itself (tens of MB RSS) reliably surfaces, command-tagged.
if has_cmd agent; then echo "  ok    process 'agent' present (command-tagged)"; else echo "  FAIL  process 'agent' missing from list"; pass=1; fi
# Identity: the kinfo_proc ruid resolves to a user name (the agent runs as freebsd), and
# the payload's OS shows on each proc.
if jq -e --arg h "$HOST_NAME" 'any(.data[]?; .attributes.host==$h and (.attributes.tags|index("os_name:freebsd")) and (.attributes.tags|index("user:freebsd")))' "$PROC_JSON" >/dev/null 2>&1; then
	echo "  ok    process info present (user:freebsd, os_name:freebsd)"
else echo "  FAIL  process info tags (user/os) missing"; pass=1; fi
# Create time: ki_start is an absolute timeval, so start must be a real time, not 1970.
if jq -e --arg h "$HOST_NAME" 'any(.data[]?; .attributes.host==$h and ((.attributes.start|startswith("1970"))|not))' "$PROC_JSON" >/dev/null 2>&1; then
	echo "  ok    process start time populated (real, not the 1970 epoch)"
else echo "  FAIL  process start times are the 1970 epoch (createTime unset)"; pass=1; fi
# host: filter via pup. This host returns only its own procs, a bogus host returns none.
RN=$(pup_proc_n "$HOST_NAME"); BN=$(pup_proc_n "${HOST_NAME}-does-not-exist")
if [ "${RN:-0}" -gt 0 ] && pup_all_match; then echo "  ok    host: filter returns only this host's processes (${RN})"; else echo "  FAIL  host: filter positive case (${RN:-0} returned)"; pass=1; fi
if [ "${BN:-0}" = 0 ]; then echo "  ok    host: filter excludes other hosts (bogus host -> 0)"; else echo "  FAIL  host: filter negative case (bogus -> ${BN})"; pass=1; fi

log "host meta (platform should be freebsd):"
ddpup infrastructure hosts list --filter="host:${HOST_NAME}" --output json 2>/dev/null |
	jq -c '.data.host_list[0].meta | {platform, agent_version, gohai: (.gohai != null)}' 2>/dev/null || true

if [ "$pass" = 0 ]; then echo "==> FREEBSD VM E2E PASS"; else echo "==> FREEBSD VM E2E FAIL"; fi
exit "$pass"
