#!/usr/bin/env bash
#
# Full end-to-end test: boot a Fedora VM, install the agent through the real
# deployment kit (dist/linux/setup.sh, the hardened datadog-agent.service running
# as dd-agent) with metrics + logs + host metadata enabled, run nginx, drive
# traffic.
#
# Two modes. With DD_API_KEY + DD_APP_KEY set it posts to real Datadog and verifies
# with the `pup` CLI (the manual mode). With DD_API_KEY unset it redirects every
# intake to a local fake intake (e2e/parity) in the guest and verifies the recording
# with `parity verify` (the automated CI mode: no keys, no pup, no network to
# Datadog). Both modes still exercise the real systemd deployment kit.
#
#   DD_API_KEY / DD_APP_KEY   set for the real-Datadog + pup mode, unset for fake intake
#   DD_SITE                   defaults to datadoghq.com (real mode only)
#   STOP_AFTER                debug knob: boot | provision | traffic | verify (default)
#
# Needs KVM (outbound network only in the real mode). The Fedora image and
# SSH key under /tmp/fedora-img and /tmp/fedora-vm are downloaded/generated if
# absent and reused on later runs (override the image with FEDORA_IMG_URL).
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
FEDORA_VER="${FEDORA_VER:-44}"
IMG_DIR=/tmp/fedora-img
BASE_IMG="$IMG_DIR/fedora${FEDORA_VER}.qcow2"
KEY_DIR=/tmp/fedora-vm
SSH_KEY="$KEY_DIR/id_ed25519"
# Fedora Cloud Base qcow2. The spin suffix changes per release, so allow an override.
FEDORA_IMG_URL="${FEDORA_IMG_URL:-https://download.fedoraproject.org/pub/fedora/linux/releases/${FEDORA_VER}/Cloud/x86_64/images/Fedora-Cloud-Base-Generic-${FEDORA_VER}-1.7.x86_64.qcow2}"
W=/tmp/microagent-vm
SSH_PORT=2222

RUN_ID="$(date +%s)$RANDOM"
HOST_NAME="microagent-vm-e2e-${RUN_ID}"
RUN_TAG="e2e_run:${RUN_ID}"
TEST_TAG="test:microagent-e2e"
INDEX_NAME="microagent-e2e"
INDEX_CREATED=0
QEMU_PID=""

SSH_COMMON="-i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o LogLevel=ERROR"
vmssh() { ssh $SSH_COMMON -p "$SSH_PORT" fedora@127.0.0.1 "$@"; }
vmscp() { scp $SSH_COMMON -P "$SSH_PORT" "$1" fedora@127.0.0.1:"$2"; }

# ddpup runs pup with an isolated HOME+config so it authenticates with the
# provided keys (not a stored OAuth session) and thus reads the same org the
# agent writes to.
PUP_HOME="$W/pup"
ddpup() { HOME="$PUP_HOME" XDG_CONFIG_HOME="$PUP_HOME" pup "$@"; }

log() { echo "==> $*"; }

cleanup() {
	log "cleanup"
	[ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null
	pkill -f "$W/overlay.qcow2" 2>/dev/null
	if [ "$INDEX_CREATED" = 1 ]; then
		ddpup api -X DELETE "v1/logs/config/indexes/${INDEX_NAME}" >/dev/null 2>&1 &&
			log "deleted temp log index ${INDEX_NAME}" ||
			log "note: could not delete log index ${INDEX_NAME}"
	fi
	rm -rf "$W"
}
trap cleanup EXIT

stop_here() { # $1 = stage just completed
	if [ "$STOP_AFTER" = "$1" ]; then log "STOP_AFTER=$1 reached"; exit 0; fi
}

command -v qemu-system-x86_64 >/dev/null || { echo "qemu missing"; exit 1; }
pkill -f "hostfwd=tcp::${SSH_PORT}-:22" 2>/dev/null && sleep 1 # kill any stale VM on our port
rm -rf "$W"; mkdir -p "$PUP_HOME" "$IMG_DIR" "$KEY_DIR"
rm -f /tmp/fedora-vm/overlay.qcow2 # reclaim the stale 3 GiB overlay
[ -r "$SSH_KEY" ] || ssh-keygen -t ed25519 -N "" -f "$SSH_KEY" -q
PUBKEY="$(cat "$SSH_KEY.pub")"
if [ ! -r "$BASE_IMG" ]; then
	log "downloading Fedora ${FEDORA_VER} cloud image"
	curl -fL --no-progress-meter --connect-timeout 15 -o "$BASE_IMG" "$FEDORA_IMG_URL" ||
		{ echo "fedora image download failed (override FEDORA_IMG_URL)"; rm -f "$BASE_IMG"; exit 1; }
fi
log "run_id=$RUN_ID host=$HOST_NAME site=$DD_SITE"

log "building static agent + dogstatsd sample"
( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 go build -tags netgo -o "$W/agent" ./cmd/agent ) ||
	{ echo "build failed"; exit 1; }
( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 go build -tags netgo -o "$W/dsdsample" ./e2e/dsdsample ) ||
	{ echo "dsdsample build failed"; exit 1; }
if [ "$FAKE" = 1 ]; then
	( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 go build -tags netgo -o "$W/parity" ./e2e/parity ) ||
		{ echo "parity build failed"; exit 1; }
fi

log "building cloud-init seed"
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
final_message: "CLOUD-INIT DONE after \$UPTIME s"
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
for _ in $(seq 1 60); do
	if vmssh true 2>/dev/null; then ssh_up=1; break; fi
	sleep 3
done
[ "$ssh_up" = 1 ] || { echo "SSH never came up; console tail:"; tail -30 "$W/console.log" 2>/dev/null; exit 1; }
log "VM up: $(vmssh 'cat /etc/fedora-release')"
stop_here boot

# On staging the org doesn't index arbitrary services, so (best-effort) create a
# narrow index for our test tag. On prod, logs are indexed by default. Skip it.
if [ "$FAKE" = 0 ] && [[ "$DD_SITE" == *datad0g* ]]; then
	body="{\"name\":\"${INDEX_NAME}\",\"filter\":{\"query\":\"${TEST_TAG}\"},\"num_retention_days\":3}"
	resp="$(echo "$body" | ddpup api -X POST "v1/logs/config/indexes" --input - 2>&1)"
	if echo "$resp" | grep -q "\"name\"" 2>/dev/null; then
		INDEX_CREATED=1; log "created log index ${INDEX_NAME}"
	else
		log "note: could not create staging log index (logs may be delivery-only)"
	fi
fi

log "installing nginx + acl"
vmssh 'sudo setenforce 0 2>/dev/null; sudo dnf install -y nginx acl >/tmp/nginx-install.log 2>&1 && sudo systemctl enable --now nginx' ||
	{ echo "nginx install failed"; vmssh 'tail -20 /tmp/nginx-install.log'; exit 1; }

# Install through the real deployment kit so the e2e exercises what we ship: the
# hardened datadog-agent.service running as the dd-agent user, the user creation,
# and the generated config. setup.sh writes the API key into the environment
# file, the site and TAGS into datadog.yaml.
log "installing the agent via dist/linux/setup.sh"
vmscp "$W/agent" "/tmp/agent"
vmssh 'mkdir -p /tmp/linux'
for f in datadog-agent.service liveprocesses.conf environment setup.sh; do
	vmscp "$ROOT/dist/linux/$f" "/tmp/linux/$f"
done
vmssh "cd /tmp/linux && sudo env PROCESS=1 TAGS='${TEST_TAG} ${RUN_TAG}' sh ./setup.sh /tmp/agent '${DD_API_KEY:-dummy}' '${DD_SITE}'" ||
	{ echo "setup.sh failed"; exit 1; }

# e2e overrides through the environment file (the documented override path): a
# fixed hostname so this run is identifiable in the Infrastructure List, and logs
# on. The yaml setup.sh wrote leaves both at their defaults.
vmssh "printf 'DD_HOSTNAME=%s\nDD_LOGS_ENABLED=true\n' '${HOST_NAME}' | sudo tee -a /etc/datadog-agent/environment >/dev/null"

# In fake mode, also redirect every intake to the in-guest recorder over loopback
# HTTP. The hardened unit permits AF_INET and sets no PrivateNetwork, so the
# dd-agent service reaches the recorder on 127.0.0.1. DD_HOSTNAME and
# DD_LOGS_ENABLED were appended above.
if [ "$FAKE" = 1 ]; then
	vmssh "sudo tee -a /etc/datadog-agent/environment >/dev/null" <<'EOF'
DD_DD_URL=http://127.0.0.1:18080
DD_LOGS_CONFIG_LOGS_DD_URL=http://127.0.0.1:18080
DD_PROCESS_CONFIG_PROCESS_DD_URL=http://127.0.0.1:18080
EOF
fi

vmssh 'sudo install -d /etc/datadog-agent/conf.d/nginx.d'
vmssh "sudo tee /etc/datadog-agent/conf.d/nginx.d/conf.yaml >/dev/null" <<'EOF'
logs:
  - type: file
    path: /var/log/nginx/access.log
    service: nginx
    source: nginx
EOF

# The hardened unit runs as dd-agent. On Debian nginx logs are root:adm and the
# unit's adm supplementary group already covers them, but Fedora keeps
# /var/log/nginx as root:root 0700, so grant dd-agent read access with an ACL the
# way an operator would, rather than loosening the unit. rX adds search on the
# directory without making the log files executable, the default entry covers a
# rotated file.
vmssh 'sudo setfacl -R -m u:dd-agent:rX /var/log/nginx && sudo setfacl -d -m u:dd-agent:rX /var/log/nginx'

# The delivery assertions read the agent's own debug log (the "...sent" lines are
# DEBUG), so add a drop-in that runs it with -debug. setup.sh's unit runs at info
# level, as a production install should.
vmssh 'sudo install -d /etc/systemd/system/datadog-agent.service.d'
vmssh "sudo tee /etc/systemd/system/datadog-agent.service.d/debug.conf >/dev/null" <<'EOF'
[Service]
ExecStart=
ExecStart=/opt/datadog-agent/bin/agent -cfgpath /etc/datadog-agent/datadog.yaml -debug
EOF

# dsdsample is a separate workload generator, not part of the kit.
vmscp "$W/dsdsample" "/tmp/dsdsample"
vmssh 'sudo install -m0755 /tmp/dsdsample /usr/local/bin/dsdsample'

# In fake mode, bring up the in-guest recorder before the restart below so it
# captures the agent's startup host metadata. Background only the simple redirected
# "nohup parity" command, after plain ";"-separated setup, not a "&&" list:
# backgrounding a "&&" list runs it in a subshell that waits for the daemon while
# holding the SSH channel open, which hangs the call.
if [ "$FAKE" = 1 ]; then
	vmscp "$W/parity" "/tmp/parity"
	vmssh 'chmod 0755 /tmp/parity; mkdir -p /tmp/dd-rec; cd /tmp; nohup /tmp/parity serve -dir dd-rec ours=127.0.0.1:18080 >/tmp/parity.log 2>&1 </dev/null & sleep 1; echo serving'
fi
vmssh 'sudo systemctl daemon-reload && sudo systemctl restart datadog-agent'
sleep 3
log "agent status: $(vmssh 'systemctl is-active datadog-agent')"
stop_here provision

log "generating nginx traffic"
vmssh "for i in \$(seq 1 25); do curl -s -o /dev/null http://localhost/; done; \
       curl -s -o /dev/null http://localhost/microagent-e2e-${RUN_ID}"

# A sample 'service' emits a full DogStatsD workload (gauge, counter, histogram,
# timing, set, plus a service check and an event) over ~25s so it spans an agent
# flush. This exercises the whole metrics forwarding pipeline, not just a gauge.
log "running dogstatsd sample service (full metric workload)"
vmssh "/usr/local/bin/dsdsample -addr 127.0.0.1:8125 -duration 25s -tags '${RUN_TAG},${TEST_TAG}' -prefix microagent.vm.dsd"
sleep 8
log "agent journal (pipeline evidence):"
vmssh 'sudo journalctl -u datadog-agent --no-pager | grep -E "agent starting|dogstatsd listening|metrics flush|log batch|service checks|events" | tail -20'
stop_here traffic

# Fake-intake mode: stop the agent (final flush) and the recorder, then assert the
# recording carries the DogStatsD workload, the Linux host metrics, host metadata
# (platform linux), and the unique log line. No pup, no real Datadog.
if [ "$FAKE" = 1 ]; then
	log "stopping agent + recorder (final flush)"
	vmssh 'sudo systemctl stop datadog-agent; sleep 2; pkill -INT -f "parity serve"; sleep 1; true'
	log "records: $(vmssh 'wc -l </tmp/dd-rec/ours.jsonl 2>/dev/null')"
	log "verifying the fake-intake recording (parity verify)"
	if vmssh "/tmp/parity verify -series datadog.agent.running,microagent.vm.dsd.gauge,microagent.vm.dsd.requests,microagent.vm.dsd.render.95percentile,microagent.vm.dsd.latency.avg,microagent.vm.dsd.users,system.cpu.idle,system.mem.total,system.disk.total,system.io.r_s,system.net.bytes_rcvd,system.load.1,system.uptime -check microagent.vm.dsd.check -event 'dsdsample up' -platform linux -meta -min-procs 10 -proc-name agent -host ${HOST_NAME} -log microagent-e2e-${RUN_ID} /tmp/dd-rec/ours.jsonl"; then
		echo "==> LINUX VM E2E (fake intake) PASS"; exit 0
	fi
	echo "==> LINUX VM E2E (fake intake) FAIL"
	echo "--- agent journal tail ---"; vmssh 'sudo journalctl -u datadog-agent --no-pager | tail -20'
	echo "--- parity log ---"; vmssh 'tail -20 /tmp/parity.log 2>/dev/null'
	exit 1
fi

metric_is_42() {
	ddpup metrics query --query="avg:microagent.vm.dsd.gauge{${RUN_TAG}}" --from=15m --output json 2>/dev/null |
		jq -e '[.data.series[]?.pointlist[]?[1] | select(. != null)] | any(. == 42)' >/dev/null
}
metric_present() {
	ddpup metrics query --query="avg:$1{${RUN_TAG}}" --from=15m --output json 2>/dev/null |
		jq -e '.data.series | length > 0' >/dev/null
}
agent_logged() { vmssh "sudo journalctl -u datadog-agent --no-pager | grep -q \"$1\""; }
host_metric_present() {
	ddpup metrics query --query="avg:system.mem.total{${RUN_TAG}}" --from=15m --output json 2>/dev/null |
		jq -e '.data.series | length > 0' >/dev/null
}
host_present() {
	# The host appears in the Infrastructure List carrying our tags and the agent
	# version + gohai from the v5 metadata payload (agent_version is only set by
	# that payload, so it proves metadata, not just metrics, was ingested).
	ddpup infrastructure hosts list --filter="host:${HOST_NAME}" --output json 2>/dev/null |
		jq -e --arg t "$RUN_TAG" '.data.host_list[0] | (.tags_by_source.Datadog | index($t)) and ((.meta.agent_version // "") != "")' >/dev/null
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
wait_for "dogstatsd gauge = 42"                      300 metric_is_42                                          || pass=1
wait_for "dogstatsd counter present"                 300 metric_present microagent.vm.dsd.requests             || pass=1
wait_for "dogstatsd histogram .95percentile present" 300 metric_present microagent.vm.dsd.render.95percentile  || pass=1
wait_for "dogstatsd set distinct-count present"      300 metric_present microagent.vm.dsd.users                || pass=1
wait_for "host metric system.mem.total present"      300 host_metric_present                                   || pass=1
wait_for "host in Infrastructure List w/ tags+metadata" 300 host_present                                       || pass=1

# Service checks and events: delivery is the assertion (the aggregator logs a 2xx),
# proving the _sc/_e parse plus the check_run and /intake/ event forwarding paths.
if agent_logged "service checks sent"; then echo "  ok    service check delivered (agent 2xx)"; else echo "  FAIL  service check not delivered"; pass=1; fi
if agent_logged "events sent"; then echo "  ok    event delivered (agent 2xx)"; else echo "  FAIL  event not delivered"; pass=1; fi

if agent_logged "log batch sent"; then
	echo "  ok    nginx logs delivered (agent 2xx)"
else
	echo "  FAIL  nginx logs not delivered by agent"; pass=1
fi
wait_for "nginx logs searchable via pup" 300 logs_searchable || pass=1

if [ "$pass" = 0 ]; then echo "==> VM E2E PASS"; else echo "==> VM E2E FAIL"; fi
exit "$pass"
