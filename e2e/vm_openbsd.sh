#!/usr/bin/env bash
#
# Full end-to-end test on OpenBSD: install OpenBSD once via an unattended autoinstall
# (cached as a qcow2), then on every run boot that image, run the cross-built
# micro-agent (metrics + logs + host metadata + Live Processes) as the unprivileged
# `puffy` user, drive a DogStatsD sample plus an nginx-format access log, and verify
# that the DogStatsD metrics, the OpenBSD host metrics (mem via vm.uvmexp, disk via
# getfsstat, cpu/load/uptime via sysctl), the logs, the host metadata (platform
# openbsd), and the process list all arrived.
#
# This is the live proof for the OpenBSD host-stats path and the OpenBSD process
# collector (KERN_PROC / struct kinfo_proc, decoded at pinned amd64 offsets).
#
# Two phases. PHASE A runs only when the installed image is absent: it PXE-boots
# bsd.rd in autoinstall mode (a boot.conf served over TFTP switches the console to
# com0, so the install is driven headless over a serial line), fetches the response
# file from a local HTTP server, pulls the sets from a mirror, and caches the result.
# PHASE B boots the cached image and is identical to the FreeBSD test from the SSH
# point on.
#
# Two modes. With DD_API_KEY + DD_APP_KEY set it posts to real Datadog and verifies
# with pup. With DD_API_KEY unset it posts to a local fake intake (e2e/parity) in the
# guest and verifies with `parity verify` (the automated CI mode: no keys, no pup).
#
#   DD_API_KEY / DD_APP_KEY   set for real Datadog + pup, unset for the fake intake
#   DD_SITE                   defaults to datadoghq.com (real mode only)
#   OPENBSD_VER               OpenBSD release to install (default 7.9)
#   OPENBSD_MIRROR            sets mirror (default https://cdn.openbsd.org/pub/OpenBSD)
#   STOP_AFTER                debug knob: install | boot | provision | traffic | verify
#
# Needs KVM, qemu, expect, socat, python3, and outbound network (run with the sandbox
# off). The installed image and an SSH key are cached under /tmp/openbsd-img.
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
VER="${OPENBSD_VER:-7.9}"
MIRROR="${OPENBSD_MIRROR:-https://cdn.openbsd.org/pub/OpenBSD}"
IMG_DIR=/tmp/openbsd-img
INSTALLED="$IMG_DIR/openbsd-${VER}-installed.qcow2"
SSH_KEY="$IMG_DIR/id_ed25519"        # baked into the image at install, so cache it with the image
W=/tmp/microagent-vm-openbsd
SSH_PORT=2224
HTTP_PORT=18082                      # host server for install.conf (PHASE A only)
GUEST_DIR=/home/puffy/dd-e2e
CAP=/tmp/openbsd-proc-cap            # persisted struct capture (outside the wiped $W)

RUN_ID="$(date +%s)$RANDOM"
HOST_NAME="microagent-obsd-e2e-${RUN_ID}"
RUN_TAG="e2e_run:${RUN_ID}"
TEST_TAG="test:microagent-e2e"
QEMU_PID=""
HTTP_PID=""

SSH_COMMON="-i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o LogLevel=ERROR"
vmssh() { ssh $SSH_COMMON -p "$SSH_PORT" puffy@127.0.0.1 "$@"; }
vmscp() { scp $SSH_COMMON -P "$SSH_PORT" "$1" puffy@127.0.0.1:"$2"; }

PUP_HOME="$W/pup"
ddpup() { HOME="$PUP_HOME" XDG_CONFIG_HOME="$PUP_HOME" pup "$@"; }

log() { echo "==> $*"; }

cleanup() {
	log "cleanup"
	[ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null
	[ -n "$HTTP_PID" ] && kill "$HTTP_PID" 2>/dev/null
	pkill -f "$W/overlay.qcow2" 2>/dev/null
	rm -rf "$W"
}
trap cleanup EXIT

stop_here() { if [ "$STOP_AFTER" = "$1" ]; then log "STOP_AFTER=$1 reached"; exit 0; fi; }

for t in qemu-system-x86_64 expect socat python3 ssh scp; do
	command -v "$t" >/dev/null || { echo "$t missing"; exit 1; }
done
pkill -f "hostfwd=tcp::${SSH_PORT}-:22" 2>/dev/null && sleep 1
rm -rf "$W"; mkdir -p "$PUP_HOME" "$IMG_DIR" "$CAP"

# SSH key (generated once, cached with the image since its pubkey is baked in).
[ -r "$SSH_KEY" ] || ssh-keygen -t ed25519 -N "" -f "$SSH_KEY" -q
PUBKEY="$(cat "$SSH_KEY.pub")"
log "run_id=$RUN_ID host=$HOST_NAME ver=$VER site=$DD_SITE fake=$FAKE"

log "building agent + dsdsample (openbsd/amd64)"
gobuild() { ( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod GOOS=openbsd GOARCH=amd64 CGO_ENABLED=0 go build -tags netgo -o "$1" "$2" ); }
gobuild "$W/agent" ./cmd/agent       || { echo "agent build failed"; exit 1; }
gobuild "$W/dsdsample" ./e2e/dsdsample || { echo "dsdsample build failed"; exit 1; }
gobuild "$W/proccap" ./e2e/proccap   || { echo "proccap build failed"; exit 1; }
[ "$FAKE" = 1 ] && { gobuild "$W/parity" ./e2e/parity || { echo "parity build failed"; exit 1; }; }

# PHASE A: unattended install, only when the cached image is absent.
if [ ! -r "$INSTALLED" ]; then
	log "no cached image, running unattended autoinstall (PHASE A)"
	command -v curl >/dev/null || { echo "curl missing"; exit 1; }
	mkdir -p "$IMG_DIR/tftp/etc" "$W/srv"
	for f in pxeboot bsd.rd; do
		[ -r "$IMG_DIR/$f" ] || curl -fL --no-progress-meter -o "$IMG_DIR/$f" "$MIRROR/$VER/amd64/$f" || { echo "$f download failed"; exit 1; }
		cp "$IMG_DIR/$f" "$IMG_DIR/tftp/$f"
	done
	# boot.conf moves the install console to com0 so we can drive it headless.
	printf 'stty com0 115200\nset tty com0\nboot tftp:bsd.rd\n' > "$IMG_DIR/tftp/etc/boot.conf"
	cp "$IMG_DIR/tftp/etc/boot.conf" "$IMG_DIR/tftp/boot.conf"
	# Split the mirror into host plus path for the set-location prompts.
	MHOST="${MIRROR#http*://}"; MPATH="${MHOST#*/}"; MHOST="${MHOST%%/*}"
	# Response file. Keys are substrings of the installer prompts. The NIC is em0
	# (we PXE-boot via -device e1000), the virtio target disk shows as sd0.
	cat > "$W/srv/install.conf" <<EOF
System hostname = obsd-e2e
Which network interface do you wish to configure = em0
IPv4 address for em0 = dhcp
IPv6 address for em0 = none
Which network interface do you wish to configure = done
Password for root = MicroAgent.e2e.1
Public ssh key for root = ${PUBKEY}
Allow root ssh login = yes
Start sshd(8) by default = yes
Change the default console to com0 = yes
Which speed should com0 use = 115200
Setup a user = puffy
Full name for user = puffy
Password for user = MicroAgent.e2e.1
Public ssh key for user = ${PUBKEY}
What timezone are you in = UTC
Which disk is the root disk = sd0
Encrypt the root disk = no
Use (W)hole disk MBR, whole disk (G)PT = whole
Use (A)uto layout, (E)dit auto layout, or create (C)ustom layout = auto
Location of sets = http
HTTP proxy URL = none
HTTP Server = ${MHOST}
Server directory = ${MPATH}/${VER}/amd64
Unable to connect using https. Use http instead = yes
Set name(s) = -x* -game*
Directory does not contain SHA256.sig. Continue without verification = yes
Continue without verification = yes
Location of sets = done
EOF
	# The mirror host/dir lines above assume a host/path mirror. Override for a bare host.
	python3 -m http.server "$HTTP_PORT" --bind 0.0.0.0 --directory "$W/srv" >"$W/httpd.log" 2>&1 &
	HTTP_PID=$!
	sleep 1
	qemu-img create -f qcow2 "$W/target.qcow2" 8G >/dev/null
	cat > "$W/install.expect" <<EXP
set timeout 1800
log_file -a $W/install.log
proc bail {m} { puts stderr "\nINSTALL-FAIL: \$m"; catch {exec kill [exp_pid]}; exit 1 }
spawn qemu-system-x86_64 -enable-kvm -cpu host -m 1024 -smp 2 \
  -drive file=$W/target.qcow2,format=qcow2,if=virtio \
  -netdev user,id=net0,tftp=$IMG_DIR/tftp,bootfile=pxeboot \
  -device e1000,netdev=net0 -boot n \
  -display none -vga std -monitor none -serial stdio
expect {
  -re {\(A\)utoinstall or \(S\)hell\?} { send "a\r"; exp_continue }
  -re {Response file location\?} { send "http://10.0.2.2:${HTTP_PORT}/install.conf\r" }
  timeout { bail "no response-file prompt" }
}
expect {
  -re {CONGRATULATIONS} { }
  -re {Response file location\?} { bail "install.conf fetch failed" }
  timeout { bail "install did not finish" }
}
# Halt cleanly so the freshly written filesystems are unmounted and synced before the disk
# is snapshotted. Killing qemu at CONGRATULATIONS leaves the root fs dirty, and PHASE B then
# drops to "Automatic file system check failed" (single-user, no sshd) instead of booting.
set timeout 180
expect { -re {\(R\)eboot} { send "halt\r" } timeout { bail "no exit prompt after install" } }
expect { -re {halted|[Pp]ress any key} { } timeout { bail "guest did not halt cleanly" } }
after 2000
catch {exec kill [exp_pid]}
puts "\nINSTALL-OK"
EXP
	expect -f "$W/install.expect" || { echo "install failed; tail:"; tail -30 "$W/install.log" 2>/dev/null; exit 1; }
	cp "$W/target.qcow2" "$INSTALLED"
	kill "$HTTP_PID" 2>/dev/null; HTTP_PID=""
	log "installed image cached at $INSTALLED"
fi
stop_here install

# PHASE B: boot the cached image. e1000 gives em0 (the installed config), so DHCP
# comes up and sshd is reachable over the host-forwarded port.
log "booting cached image (PHASE B)"
qemu-img create -f qcow2 -b "$INSTALLED" -F qcow2 "$W/overlay.qcow2" >/dev/null 2>&1
qemu-system-x86_64 -enable-kvm -cpu host -m 1024 -smp 2 \
	-drive file="$W/overlay.qcow2",format=qcow2,if=virtio \
	-netdev user,id=net0,hostfwd=tcp::${SSH_PORT}-:22 -device e1000,netdev=net0 \
	-display none -vga std -serial file:"$W/console.log" </dev/null >"$W/qemu.out" 2>&1 &
QEMU_PID=$!

log "waiting for SSH"
ssh_up=0
for _ in $(seq 1 80); do
	if vmssh true 2>/dev/null; then ssh_up=1; break; fi
	sleep 3
done
[ "$ssh_up" = 1 ] || { echo "SSH never came up; console tail:"; tail -40 "$W/console.log" 2>/dev/null; exit 1; }
log "VM up: $(vmssh 'uname -srm')"

# Capture the live process struct layout (the analog of vm_freebsd's kinfo_proc dump),
# persisted outside $W so the parser offsets can be checked against this exact release.
vmssh "mkdir -p $GUEST_DIR"
vmscp "$W/proccap" "$GUEST_DIR/proccap"
vmssh "chmod 0755 $GUEST_DIR/proccap; $GUEST_DIR/proccap >$GUEST_DIR/proc.bin 2>$GUEST_DIR/proc.meta; cat $GUEST_DIR/proc.meta"
vmssh "cat $GUEST_DIR/proc.bin" > "$CAP/proc.bin" 2>/dev/null
vmssh "cat $GUEST_DIR/proc.meta" > "$CAP/proc.meta" 2>/dev/null
vmssh "sysctl hw.ncpu hw.physmem kern.osrelease 2>/dev/null; ps -axo pid,ppid,ruser,rss,stat,comm 2>/dev/null | head -20" > "$CAP/info.txt" 2>/dev/null
log "process struct capture: $(cat "$CAP/proc.meta" 2>/dev/null)"

# Disk-I/O struct capture: compile a tiny offdump in the guest (cc is in base) that prints
# the struct diskstats offsets plus the live HW_DISKSTATS device list, the ground truth for
# the pinned offsets in diskstatsbsd.go, and writes the raw blob back for the golden test.
vmssh "cat > $GUEST_DIR/diskoff.c" <<'CEOF'
#include <sys/types.h>
#include <sys/sysctl.h>
#include <sys/disk.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
int main(void){
	printf("OBSD-OFF sizeof=%zu name=%zu rxfer=%zu wxfer=%zu rbytes=%zu wbytes=%zu time=%zu NAMELEN=%d\n",
		sizeof(struct diskstats), offsetof(struct diskstats,ds_name), offsetof(struct diskstats,ds_rxfer),
		offsetof(struct diskstats,ds_wxfer), offsetof(struct diskstats,ds_rbytes),
		offsetof(struct diskstats,ds_wbytes), offsetof(struct diskstats,ds_time), DS_DISKNAMELEN);
	int mib[2]={CTL_HW,HW_DISKSTATS}; size_t n=0;
	if(sysctl(mib,2,NULL,&n,NULL,0)){perror("size");return 1;}
	char*b=malloc(n); if(sysctl(mib,2,b,&n,NULL,0)){perror("get");return 1;}
	size_t recs=n/sizeof(struct diskstats);
	printf("OBSD-BLOB bytes=%zu recs=%zu\n", n, recs);
	for(size_t i=0;i<recs;i++){ struct diskstats*d=(struct diskstats*)(b+i*sizeof(struct diskstats));
		printf("OBSD-DEV %s rxfer=%llu wxfer=%llu rbytes=%llu\n", d->ds_name,
			(unsigned long long)d->ds_rxfer,(unsigned long long)d->ds_wxfer,(unsigned long long)d->ds_rbytes); }
	FILE*f=fopen("diskstats.bin","wb"); if(f){fwrite(b,1,n,f);fclose(f);}
	return 0;
}
CEOF
vmssh "cd $GUEST_DIR; cc -o diskoff diskoff.c 2>diskoff.err && ./diskoff || cat diskoff.err"
vmssh "cat $GUEST_DIR/diskstats.bin" > "$CAP/diskstats.bin" 2>/dev/null

stop_here boot

log "installing micro-agent (unprivileged, under $GUEST_DIR)"
vmssh "mkdir -p $GUEST_DIR/conf.d/nginx.d $GUEST_DIR/run && : > $GUEST_DIR/access.log"
vmscp "$W/agent" "$GUEST_DIR/agent"
vmscp "$W/dsdsample" "$GUEST_DIR/dsdsample"
vmssh "chmod 0755 $GUEST_DIR/agent $GUEST_DIR/dsdsample"

if [ "$FAKE" = 1 ]; then
	vmscp "$W/parity" "$GUEST_DIR/parity"; vmssh "chmod 0755 $GUEST_DIR/parity"
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

# Start the daemons with "cd dir; nohup cmd ... &", not "cd dir && cmd &". OpenBSD's
# ksh backgrounds a "&&" list as a subshell that waits for the daemon while holding
# the SSH channel open, which hangs the call. Backgrounding the simple redirected
# command (after a plain cd) lets the SSH session close at once.
if [ "$FAKE" = 1 ]; then
	vmssh "cd $GUEST_DIR; nohup ./parity serve -dir rec ours=127.0.0.1:18080 >parity.log 2>&1 </dev/null & echo serving"
fi
vmssh "cd $GUEST_DIR; nohup ./agent --cfgpath datadog.yaml --debug >agent.log 2>&1 </dev/null & echo started"
sleep 3
log "agent log (head):"; vmssh "head -12 $GUEST_DIR/agent.log"
stop_here provision

# traffic. The window spans >=2 agent flushes so the cpu rate collector emits.
log "writing nginx-format access log + running dogstatsd sample"
vmssh "cd $GUEST_DIR; \
       for i in \$(seq 1 25); do printf '127.0.0.1 - - [%s] \"GET / HTTP/1.1\" 200 12 \"-\" \"e2e\"\n' \"\$(date '+%d/%b/%Y:%H:%M:%S %z')\" >> access.log; done; \
       printf '127.0.0.1 - - [%s] \"GET /microagent-e2e-${RUN_ID} HTTP/1.1\" 200 5 \"-\" \"e2e\"\n' \"\$(date '+%d/%b/%Y:%H:%M:%S %z')\" >> access.log"
vmssh "$GUEST_DIR/dsdsample -addr 127.0.0.1:8125 -duration 35s -tags '${RUN_TAG},${TEST_TAG}' -prefix microagent.vm.dsd"
sleep 10
log "agent log (tail):"; vmssh "tail -12 $GUEST_DIR/agent.log"
stop_here traffic

if [ "$FAKE" = 1 ]; then
	log "stopping agent + recorder (final flush)"
	vmssh "pkill -TERM -f 'agent --cfgpath'; sleep 3; pkill -INT -f 'parity serve'; sleep 1; true"
	log "records: $(vmssh "wc -l <$GUEST_DIR/rec/ours.jsonl 2>/dev/null")"
	if vmssh "$GUEST_DIR/parity verify \
		-series datadog.agent.running,microagent.vm.dsd.gauge,microagent.vm.dsd.requests,microagent.vm.dsd.render.95percentile,microagent.vm.dsd.latency.avg,microagent.vm.dsd.users,system.mem.total,system.disk.total,system.load.1,system.uptime,system.io.r_s,system.io.util \
		-check microagent.vm.dsd.check -event 'dsdsample up' \
		-platform openbsd -meta -host ${HOST_NAME} \
		-min-procs 10 -proc-name agent \
		-log microagent-e2e-${RUN_ID} \
		$GUEST_DIR/rec/ours.jsonl"; then
		echo "==> OPENBSD VM E2E (fake intake) PASS"; exit 0
	fi
	echo "==> OPENBSD VM E2E (fake intake) FAIL"
	echo "--- agent log tail ---"; vmssh "tail -20 $GUEST_DIR/agent.log 2>/dev/null"
	exit 1
fi

metric_present() { ddpup metrics query --query="avg:$1{${RUN_TAG}}" --from=15m --output json 2>/dev/null | jq -e '.data.series | length > 0' >/dev/null; }
metric_is_42() { ddpup metrics query --query="avg:microagent.vm.dsd.gauge{${RUN_TAG}}" --from=15m --output json 2>/dev/null | jq -e '[.data.series[]?.pointlist[]?[1]|select(.!=null)]|any(.==42)' >/dev/null; }
host_present() { ddpup infrastructure hosts list --filter="host:${HOST_NAME}" --output json 2>/dev/null | jq -e --arg t "$RUN_TAG" '.data.host_list[0]|(.tags_by_source.Datadog|index($t)) and ((.meta.agent_version // "")!="")' >/dev/null; }
logs_searchable() { ddpup logs search --query="service:nginx ${RUN_TAG}" --from=20m --limit=10 --output json 2>/dev/null | jq -e '.data|length>0' >/dev/null; }
wait_for() { local desc=$1 timeout=$2; shift 2; local waited=0; until "$@"; do waited=$((waited+10)); if [ "$waited" -gt "$timeout" ]; then echo "  FAIL  $desc (timeout ${waited}s)"; return 1; fi; sleep 10; done; echo "  ok    $desc"; }

log "verifying via pup (ingestion latency ~minutes)"
pass=0
wait_for "dogstatsd gauge = 42"                300 metric_is_42 || pass=1
wait_for "dogstatsd counter present"           300 metric_present microagent.vm.dsd.requests || pass=1
wait_for "dogstatsd set distinct-count present" 300 metric_present microagent.vm.dsd.users || pass=1
wait_for "host metric system.mem.total"        300 metric_present system.mem.total || pass=1
wait_for "host metric system.disk.total"       300 metric_present system.disk.total || pass=1
wait_for "host metric system.load.1"           300 metric_present system.load.1 || pass=1
for line in 'host metadata sent' 'log batch sent' 'service checks sent' 'events sent' 'process payload sent'; do
	if vmssh "grep -q '$line' $GUEST_DIR/agent.log"; then echo "  ok    delivered: $line"; else echo "  FAIL  not delivered: $line"; pass=1; fi
done
hp=0; for _ in $(seq 1 18); do host_present && { hp=1; break; }; sleep 10; done
[ "$hp" = 1 ] && echo "  ok    host in Infrastructure List" || echo "  ..    host pending (indexing lag)"
wait_for "logs searchable via pup" 300 logs_searchable || pass=1
PROC_JSON="$W/processes.json"
curl -s -m 30 -H "DD-API-KEY: ${DD_API_KEY}" -H "DD-APPLICATION-KEY: ${DD_APP_KEY}" "https://api.${DD_SITE}/api/v2/processes?page%5Blimit%5D=1000" >"$PROC_JSON" 2>/dev/null
N=$(jq --arg h "$HOST_NAME" '[.data[]?|select(.attributes.host==$h)]|length' "$PROC_JSON" 2>/dev/null || echo 0)
if [ "${N:-0}" -ge 10 ]; then echo "  ok    Live Processes shows $N processes"; else echo "  FAIL  Live Processes shows ${N:-0} (<10)"; pass=1; fi
if jq -e --arg h "$HOST_NAME" 'any(.data[]?; .attributes.host==$h and (.attributes.tags|index("os_name:openbsd")) and (.attributes.tags|index("user:puffy")))' "$PROC_JSON" >/dev/null 2>&1; then echo "  ok    process info (user:puffy, os_name:openbsd)"; else echo "  FAIL  process info tags missing"; pass=1; fi

if [ "$pass" = 0 ]; then echo "==> OPENBSD VM E2E PASS"; else echo "==> OPENBSD VM E2E FAIL"; fi
exit "$pass"
