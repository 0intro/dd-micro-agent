#!/usr/bin/env bash
#
# Full end-to-end test on NetBSD: boot the official NetBSD live image, run the
# cross-built micro-agent (metrics + logs + host metadata + Live Processes) as root,
# drive a DogStatsD sample plus an nginx-format access log, and verify that the
# DogStatsD metrics, the NetBSD host metrics (mem via vm.uvmexp2, disk via getvfsstat,
# cpu/load/uptime via sysctl), the logs, the host metadata (platform netbsd), and the
# process list all arrived.
#
# This is the live proof for the NetBSD host-stats path and the NetBSD process
# collector (KERN_PROC2 / struct kinfo_proc2, decoded at pinned amd64 offsets).
#
# The live image boots to a VGA console, so there is no cloud-init and no SSH key to
# inject. The console is moved to com0 over the boot menu with expect (one SPACE stops
# the countdown, "3" drops to the boot prompt, "consdev com0" switches the console),
# then the whole guest workflow runs from a single fetched script so the serial driver
# only has to log in, launch it, and wait for one sentinel. The cross-built binaries
# and config come from a small host HTTP server over the SLIRP gateway (10.0.2.2), and
# the verifier runs in the guest.
#
# Keyless fake-intake mode only (e2e/parity in the guest): no Datadog keys, no pup.
# Real-Datadog verification on the BSDs is covered by vm_freebsd / vm_openbsd.
#
#   NETBSD_IMG_URL   override the live-image URL
#   STOP_AFTER       debug knob: build (stop before booting the VM) | verify (default)
#
# Needs KVM, qemu, expect, socat, python3, gzip, and outbound network (run with the
# sandbox off). The live image is downloaded and cached under /tmp/netbsd-img.
set -uo pipefail

NETBSD_VER=10.1
IMG_URL="${NETBSD_IMG_URL:-https://cdn.netbsd.org/pub/NetBSD/images/${NETBSD_VER}/NetBSD-${NETBSD_VER}-amd64-live.img.gz}"
IMG_DIR=/tmp/netbsd-img
BASE_IMG="$IMG_DIR/NetBSD-${NETBSD_VER}-amd64-live.img"
W=/tmp/microagent-vm-netbsd
HTTP_PORT=18081
CAP=/tmp/netbsd-proc-cap
STOP_AFTER="${STOP_AFTER:-verify}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="$(date +%s)$RANDOM"
HOST_NAME="microagent-nbsd-e2e-${RUN_ID}"
RUN_TAG="e2e_run:${RUN_ID}"
TEST_TAG="test:microagent-e2e"
HTTP_PID=""

log() { echo "==> $*"; }

cleanup() {
	log "cleanup"
	[ -n "$HTTP_PID" ] && kill "$HTTP_PID" 2>/dev/null
	[ -S "$W/mon.sock" ] && echo quit | socat - "UNIX-CONNECT:$W/mon.sock" 2>/dev/null
	for p in $(ps -C qemu-system-x86_64 -o pid= 2>/dev/null); do
		tr '\0' ' ' </proc/"$p"/cmdline 2>/dev/null | grep -qa "$W/overlay" && kill "$p" 2>/dev/null
	done
	rm -rf "$W"
}
trap cleanup EXIT

stop_here() { if [ "$STOP_AFTER" = "$1" ]; then log "STOP_AFTER=$1 reached"; exit 0; fi; }

for t in qemu-system-x86_64 expect socat python3 gzip; do
	command -v "$t" >/dev/null || { echo "$t missing"; exit 1; }
done
rm -rf "$W"; mkdir -p "$W/srv/conf.d/nginx.d" "$CAP" "$IMG_DIR"

if [ ! -r "$BASE_IMG" ]; then
	command -v curl >/dev/null || { echo "curl missing"; exit 1; }
	log "downloading NetBSD ${NETBSD_VER} live image"
	curl -fL --no-progress-meter -o "$BASE_IMG.gz" "$IMG_URL" || { echo "image download failed"; exit 1; }
	gzip -df "$BASE_IMG.gz" || { echo "image decompress failed"; exit 1; }
fi
log "run_id=$RUN_ID host=$HOST_NAME"

log "building agent + dsdsample + parity + proccap (netbsd/amd64)"
gobuild() { ( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod GOOS=netbsd GOARCH=amd64 CGO_ENABLED=0 go build -tags netgo -o "$1" "$2" ); }
gobuild "$W/srv/agent" ./cmd/agent       || { echo "agent build failed"; exit 1; }
gobuild "$W/srv/dsdsample" ./e2e/dsdsample || { echo "dsdsample build failed"; exit 1; }
gobuild "$W/srv/parity" ./e2e/parity     || { echo "parity build failed"; exit 1; }
gobuild "$W/srv/proccap" ./e2e/proccap   || { echo "proccap build failed"; exit 1; }

cat > "$W/srv/datadog.yaml" <<EOF
api_key: dummy
dd_url: http://127.0.0.1:18080
hostname: ${HOST_NAME}
tags:
  - ${TEST_TAG}
  - ${RUN_TAG}
logs_enabled: true
enable_metadata_collection: true
run_path: /root/dd-e2e/run
confd_path: /root/dd-e2e/conf.d
logs_config: {logs_dd_url: http://127.0.0.1:18080}
process_config:
  process_dd_url: http://127.0.0.1:18080
  process_collection:
    enabled: true
EOF
cat > "$W/srv/conf.d/nginx.d/conf.yaml" <<EOF
logs:
  - type: file
    path: /root/dd-e2e/access.log
    service: nginx
    source: nginx
EOF
{
	for _ in $(seq 1 25); do
		printf '127.0.0.1 - - [x] "GET / HTTP/1.1" 200 12 "-" "e2e"\n'
	done
	printf '127.0.0.1 - - [x] "GET /microagent-e2e-%s HTTP/1.1" 200 5 "-" "e2e"\n' "$RUN_ID"
} > "$W/srv/access.seed"

# NetBSD disk-I/O offset capture, compiled in the guest (cc is in base): the ground truth
# for the struct io_sysctl offsets pinned in diskstatsbsd.go, plus the raw HW_IOSTATS blob.
cat > "$W/srv/diskoff.c" <<'CEOF'
#include <sys/types.h>
#include <sys/sysctl.h>
#include <sys/iostat.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
int main(void){
	printf("NBSD-OFF sizeof=%zu name=%zu type=%zu time=%zu rxfer=%zu rbytes=%zu wxfer=%zu wbytes=%zu NAMELEN=%d\n",
		sizeof(struct io_sysctl), offsetof(struct io_sysctl,name), offsetof(struct io_sysctl,type),
		offsetof(struct io_sysctl,time_sec), offsetof(struct io_sysctl,rxfer), offsetof(struct io_sysctl,rbytes),
		offsetof(struct io_sysctl,wxfer), offsetof(struct io_sysctl,wbytes), IOSTATNAMELEN);
	int mib[3]={CTL_HW,HW_IOSTATS,sizeof(struct io_sysctl)}; size_t n=0;
	if(sysctl(mib,3,NULL,&n,NULL,0)){perror("size");return 1;}
	char*b=malloc(n); if(sysctl(mib,3,b,&n,NULL,0)){perror("get");return 1;}
	size_t recs=n/sizeof(struct io_sysctl);
	printf("NBSD-BLOB bytes=%zu recs=%zu\n", n, recs);
	for(size_t i=0;i<recs;i++){ struct io_sysctl*d=(struct io_sysctl*)(b+i*sizeof(struct io_sysctl));
		printf("NBSD-DEV %s type=%d rxfer=%llu wxfer=%llu rbytes=%llu\n", d->name, (int)d->type,
			(unsigned long long)d->rxfer,(unsigned long long)d->wxfer,(unsigned long long)d->rbytes); }
	FILE*f=fopen("iostats.bin","wb"); if(f){fwrite(b,1,n,f);fclose(f);}
	return 0;
}
CEOF

# The whole guest workflow as one script. Running it from a single command keeps the
# serial driver from having to sync on a prompt after every step (NetBSD daemons emit
# async console lines, and the slow boot makes interactive matching brittle). It ends
# by printing a unique sentinel the driver waits for.
cat > "$W/srv/run.sh" <<EOF
#!/bin/sh
P=http://10.0.2.2:${HTTP_PORT}
mkdir -p /root/dd-e2e/conf.d/nginx.d /root/dd-e2e/run
cd /root/dd-e2e || { echo NBSD-RUN-9X7 fail; exit 1; }
for f in agent dsdsample parity proccap diskoff.c datadog.yaml access.seed; do
	ftp -4 -o \$f \$P/\$f || { echo NBSD-FETCH-9X7; exit 1; }
done
ftp -4 -o conf.d/nginx.d/conf.yaml \$P/conf.d/nginx.d/conf.yaml
chmod 0755 agent dsdsample parity proccap
: > access.log
./proccap >proc.bin 2>proc.meta; cat proc.meta
cc -o diskoff diskoff.c 2>diskoff.err && ./diskoff || cat diskoff.err
echo NBSD-IO-B64-START; openssl base64 -in iostats.bin 2>/dev/null || uuencode -m iostats.bin iostats.bin; echo NBSD-IO-B64-END
./parity serve -dir rec ours=127.0.0.1:18080 >parity.log 2>&1 &
./agent --cfgpath datadog.yaml --debug >agent.log 2>&1 &
sleep 3
cat access.seed >> access.log
./dsdsample -addr 127.0.0.1:8125 -duration 35s -tags '${RUN_TAG},${TEST_TAG}' -prefix microagent.vm.dsd
sleep 10
pkill agent; sleep 3; pkill parity; sleep 1
echo "records: \$(wc -l < rec/ours.jsonl)"
if ./parity verify -series datadog.agent.running,microagent.vm.dsd.gauge,microagent.vm.dsd.requests,microagent.vm.dsd.render.95percentile,microagent.vm.dsd.latency.avg,microagent.vm.dsd.users,system.mem.total,system.disk.total,system.load.1,system.uptime,system.io.r_s,system.io.util -check microagent.vm.dsd.check -event 'dsdsample up' -platform netbsd -meta -host ${HOST_NAME} -min-procs 10 -proc-name agent -log microagent-e2e-${RUN_ID} rec/ours.jsonl; then
	echo NBSD-PASS-9X7
else
	echo NBSD-FAIL-9X7
fi
EOF

python3 -m http.server "$HTTP_PORT" --bind 0.0.0.0 --directory "$W/srv" >"$W/httpd.log" 2>&1 &
HTTP_PID=$!
sleep 1

qemu-img create -f qcow2 -b "$BASE_IMG" -F raw "$W/overlay.qcow2" >/dev/null 2>&1

# The serial driver: move the console to com0, log in, fetch and run the workflow
# script, and wait for the sentinel. Shell vars (W/HTTP_PORT) are expanded here. Every
# expect "$" is escaped as \$ and \r is a literal carriage return.
cat > "$W/drive.expect" <<EXP
set timeout 240
log_file -a $W/serial.log
proc mon {cmd} { exec sh -c "echo '\$cmd' | socat - UNIX-CONNECT:$W/mon.sock" }
proc typestr {s} {
  foreach c [split \$s ""] {
    switch -- \$c { " " {set k spc} "\r" {set k ret} default {set k \$c} }
    mon "sendkey \$k"; after 70
  }
}
proc dbg {m} { puts "DBG: \$m"; flush stdout }
proc bail {m} { puts "\nDRIVE-FAIL: \$m"; flush stdout; catch {mon "screendump $W/fail.ppm"}; catch {exec kill [exp_pid]}; exit 1 }

spawn qemu-system-x86_64 -enable-kvm -cpu host -m 2048 -smp 2 \
  -drive file=$W/overlay.qcow2,format=qcow2,if=virtio \
  -netdev user,id=net0 -device virtio-net-pci,netdev=net0 \
  -display none -vga std -monitor unix:$W/mon.sock,server,nowait -serial stdio

after 3300; mon "sendkey spc"
after 700;  typestr "3\r"
after 1300; typestr "consdev com0\r"
after 1600; send "boot\r"
dbg "sent boot (kernel load over the virtio disk is slow, be patient)"
# Poll to a root shell: the first login: is buried at the end of tens of KB of boot
# output, so nudge with a newline for a fresh, short prompt, log in once, and stop
# when the shell prompt returns.
set timeout 6
set ready 0; set sent_root 0
for {set i 0} {\$i < 60} {incr i} {
  send "\r"
  expect {
    -re {login:} { if {!\$sent_root} { send "root\r"; set sent_root 1 } }
    -re {# \$}   { set ready 1 }
    timeout      {}
  }
  if {\$ready} break
}
if {!\$ready} { bail "no root shell after boot" }
dbg "got root shell"
# Configure the virtio NIC over serial before fetching anything. The live image's
# dhcpcd is unreliable (control_free errors, no lease), and SLIRP always assigns the
# guest 10.0.2.15 with gateway 10.0.2.2, so a static address is deterministic. NetBSD
# ifconfig wants a CIDR or hex mask, not a dotted one.
# Disable IPv4 DAD first, otherwise the address sits TENTATIVE (unusable) for a
# couple of seconds while duplicate-address detection runs and the first fetch fails.
send "sysctl -w net.inet.ip.dad_count=0; pkill dhcpcd; sleep 1; ifconfig vioif0 inet 10.0.2.15/24 up; route -n add default 10.0.2.2\r"
after 2000
send "ifconfig vioif0 | grep 'inet '; echo NETCFG9X\r"
expect { -re {NETCFG9X} {} timeout {} }
dbg "configured network"
# Fetch the workflow script, capturing any ftp error to the serial for diagnosis.
send "ftp -4 -o /tmp/run.sh http://10.0.2.2:${HTTP_PORT}/run.sh 2>&1; echo FTPRC=\$?X\r"
expect {
  -re {FTPRC=0X}    {}
  -re {FTPRC=[1-9]} { bail "run.sh fetch failed (ftp error in serial tail)" }
  timeout           { bail "fetch timeout" }
}
dbg "fetched run.sh"
send "sh /tmp/run.sh\r"
set timeout 200
expect {
  -re {NBSD-PASS-9X7}  { dbg "verify pass" }
  -re {NBSD-FAIL-9X7}  { bail "parity verify failed in guest" }
  -re {NBSD-FETCH-9X7} { bail "guest could not fetch binaries from the host" }
  timeout              { bail "workflow timed out" }
}
send "halt -p\r"
after 1500
catch {exec kill [exp_pid]}
exit 0
EXP

log "booting NetBSD live image and driving over serial"
stop_here build
if expect -f "$W/drive.expect"; then
	grep -a 'stride=' "$W/serial.log" 2>/dev/null | tail -1 | sed 's/^/==> process struct capture: /'
	grep -aE 'NBSD-OFF|NBSD-DEV' "$W/serial.log" 2>/dev/null | sed 's/^/==> /'
	sed -n '/NBSD-IO-B64-START/,/NBSD-IO-B64-END/p' "$W/serial.log" 2>/dev/null | grep -avE 'NBSD-IO-B64' | tr -d '\r' | openssl base64 -d > "$CAP/iostats.bin" 2>/dev/null
	cp "$W/serial.log" "$CAP/serial.log" 2>/dev/null
	echo "==> NETBSD VM E2E (fake intake) PASS"; exit 0
fi
echo "==> NETBSD VM E2E (fake intake) FAIL"
echo "--- serial tail ---"; tail -40 "$W/serial.log" 2>/dev/null | tr -d '\r' | sed 's/\x08//g'
exit 1
