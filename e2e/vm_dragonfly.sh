#!/usr/bin/env bash
#
# Full end-to-end test on DragonFly BSD: boot the official release disk image, run the
# cross-built micro-agent (metrics + logs + host metadata + Live Processes) as root,
# drive a DogStatsD sample plus an nginx-format access log, and verify that the
# DogStatsD metrics, the DragonFly host metrics (mem via hw.physmem plus vm.stats, disk
# via getfsstat, disk I/O via kern.devstat.all, cpu/load/uptime via sysctl), the logs, the
# host metadata (platform dragonfly), and the process list all arrived.
#
# This is the live proof for the DragonFly host-stats path and the DragonFly process
# collector (kern.proc.all / struct kinfo_proc, decoded at pinned amd64 offsets). The
# guest also compiles a tiny offdump.c that prints the kinfo_proc field offsets and a
# base64 of a real kern.proc.all blob, so the pinned offsets stay verifiable and the unit
# test has a captured fixture. The image boots to a VGA console with no cloud-init and no
# SSH, so the console is moved to com0 at the loader menu with expect (option 9 escapes to
# the loader prompt, "set console=comconsole" switches the console), then multi-user boots
# over serial and root logs in with no password. The cross-built binaries and config come
# from a small host HTTP server over the SLIRP gateway (10.0.2.2), and the verifier runs
# in the guest.
#
# Keyless fake-intake mode only (e2e/parity in the guest): no Datadog keys, no pup.
# Real-Datadog verification on the BSDs is covered by vm_freebsd / vm_openbsd.
#
#   DRAGONFLY_IMG_URL   override the release-image URL
#   STOP_AFTER          debug knob: build (stop before booting) | capture (stop after the
#                       struct capture) | verify (default)
#
# Needs KVM, qemu, expect, socat, python3, bzip2, and outbound network (run with the
# sandbox off). The image is downloaded and cached under /tmp/dragonfly-img.
set -uo pipefail

DFLY_VER=6.4.2
IMG_URL="${DRAGONFLY_IMG_URL:-https://mirror-master.dragonflybsd.org/iso-images/dfly-x86_64-${DFLY_VER}_REL.img.bz2}"
IMG_DIR=/tmp/dragonfly-img
BASE_IMG="$IMG_DIR/dfly-x86_64-${DFLY_VER}_REL.img"
W=/tmp/microagent-vm-dragonfly
HTTP_PORT=18082
CAP=/tmp/dragonfly-proc-cap
STOP_AFTER="${STOP_AFTER:-verify}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="$(date +%s)$RANDOM"
HOST_NAME="microagent-dfly-e2e-${RUN_ID}"
RUN_TAG="e2e_run:${RUN_ID}"
TEST_TAG="test:microagent-e2e"
HTTP_PID=""

GSTAGE=verify
[ "$STOP_AFTER" = capture ] && GSTAGE=capture

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

for t in qemu-system-x86_64 expect socat python3 bzip2; do
	command -v "$t" >/dev/null || { echo "$t missing"; exit 1; }
done
rm -rf "$W"; mkdir -p "$W/srv/conf.d/nginx.d" "$CAP"

if [ ! -r "$BASE_IMG" ]; then
	command -v curl >/dev/null || { echo "curl missing"; exit 1; }
	mkdir -p "$IMG_DIR"
	log "downloading DragonFly ${DFLY_VER} image"
	curl -fL --no-progress-meter -o "$BASE_IMG.bz2" "$IMG_URL" || { echo "image download failed"; exit 1; }
	bzip2 -df "$BASE_IMG.bz2" || { echo "image decompress failed"; exit 1; }
fi
log "run_id=$RUN_ID host=$HOST_NAME stage=$GSTAGE"

log "building agent + dsdsample + parity (dragonfly/amd64)"
gobuild() { ( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod GOOS=dragonfly GOARCH=amd64 CGO_ENABLED=0 go build -tags netgo -o "$1" "$2" ); }
gobuild "$W/srv/agent" ./cmd/agent       || { echo "agent build failed"; exit 1; }
gobuild "$W/srv/dsdsample" ./e2e/dsdsample || { echo "dsdsample build failed"; exit 1; }
gobuild "$W/srv/parity" ./e2e/parity     || { echo "parity build failed"; exit 1; }

# offdump.c prints the kinfo_proc field offsets (amd64), the sole ground truth the pinned
# offsets in kinfoprocdf.go are checked against. sys/user.h must precede sys/kinfo.h.
cat > "$W/srv/offdump.c" <<'EOF'
#include <sys/param.h>
#include <sys/types.h>
#include <sys/user.h>
#include <sys/kinfo.h>
#include <sys/resource.h>
#include <sys/devicestat.h>
#include <stddef.h>
#include <stdio.h>

#define O(f)  (unsigned long)offsetof(struct kinfo_proc, f)
#define RU(f) (unsigned long)(offsetof(struct kinfo_proc, kp_ru) + offsetof(struct rusage, f))
#define L(f)  (unsigned long)(offsetof(struct kinfo_proc, kp_lwp) + offsetof(struct kinfo_lwp, f))
#define D(f)  (unsigned long)offsetof(struct devstat, f)

int main(void) {
	printf("sizeof_kinfo_proc=%lu\n", (unsigned long)sizeof(struct kinfo_proc));
	printf("sizeof_kinfo_lwp=%lu\n", (unsigned long)sizeof(struct kinfo_lwp));
	printf("sizeof_segsz_t=%lu\n", (unsigned long)sizeof(segsz_t));
	printf("MAXCOMLEN=%d\n", MAXCOMLEN);
	printf("kp_stat=%lu\n", O(kp_stat));
	printf("kp_start=%lu\n", O(kp_start));
	printf("kp_comm=%lu\n", O(kp_comm));
	printf("kp_uid=%lu\n", O(kp_uid));
	printf("kp_ruid=%lu\n", O(kp_ruid));
	printf("kp_rgid=%lu\n", O(kp_rgid));
	printf("kp_pid=%lu\n", O(kp_pid));
	printf("kp_ppid=%lu\n", O(kp_ppid));
	printf("kp_nthreads=%lu\n", O(kp_nthreads));
	printf("kp_vm_map_size=%lu\n", O(kp_vm_map_size));
	printf("kp_vm_rssize=%lu\n", O(kp_vm_rssize));
	printf("kp_ru=%lu\n", O(kp_ru));
	printf("ru_utime=%lu\n", RU(ru_utime));
	printf("ru_stime=%lu\n", RU(ru_stime));
	printf("ru_nvcsw=%lu\n", RU(ru_nvcsw));
	printf("ru_nivcsw=%lu\n", RU(ru_nivcsw));
	printf("kp_lwp=%lu\n", O(kp_lwp));
	printf("kl_stat=%lu\n", L(kl_stat));
	printf("sizeof_devstat=%lu\n", (unsigned long)sizeof(struct devstat));
	printf("DEVSTAT_VERSION=%d\n", DEVSTAT_VERSION);
	printf("ds_device_name=%lu\n", D(device_name));
	printf("ds_unit_number=%lu\n", D(unit_number));
	printf("ds_bytes_read=%lu\n", D(bytes_read));
	printf("ds_bytes_written=%lu\n", D(bytes_written));
	printf("ds_num_reads=%lu\n", D(num_reads));
	printf("ds_num_writes=%lu\n", D(num_writes));
	printf("ds_busy_time=%lu\n", D(busy_time));
	return 0;
}
EOF

cat > "$W/srv/datadog.yaml" <<EOF
api_key: dummy
dd_url: http://127.0.0.1:18080
hostname: ${HOST_NAME}
tags:
  - ${TEST_TAG}
  - ${RUN_TAG}
logs_enabled: true
enable_metadata_collection: true
run_path: /tmp/dd-e2e/run
confd_path: /tmp/dd-e2e/conf.d
logs_config: {logs_dd_url: http://127.0.0.1:18080}
process_config:
  process_dd_url: http://127.0.0.1:18080
  process_collection:
    enabled: true
EOF
cat > "$W/srv/conf.d/nginx.d/conf.yaml" <<EOF
logs:
  - type: file
    path: /tmp/dd-e2e/access.log
    service: nginx
    source: nginx
EOF
{
	for _ in $(seq 1 25); do
		printf '127.0.0.1 - - [x] "GET / HTTP/1.1" 200 12 "-" "e2e"\n'
	done
	printf '127.0.0.1 - - [x] "GET /microagent-e2e-%s HTTP/1.1" 200 5 "-" "e2e"\n' "$RUN_ID"
} > "$W/srv/access.seed"

# The whole guest workflow as one script (see vm_netbsd.sh for the rationale). It captures
# the kinfo_proc layout first, then (unless STAGE=capture) runs the agent and verifies,
# ending with a unique sentinel the serial driver waits for.
cat > "$W/srv/run.sh" <<EOF
#!/bin/sh
STAGE=${GSTAGE}
P=http://10.0.2.2:${HTTP_PORT}
D=/tmp/dd-e2e
mkdir -p \$D/conf.d/nginx.d \$D/run
cd \$D || { echo DFLY-RUN-9X7; exit 1; }
for f in agent dsdsample parity offdump.c datadog.yaml access.seed; do
	fetch -o \$f \$P/\$f || { echo DFLY-FETCH-9X7; exit 1; }
done
fetch -o conf.d/nginx.d/conf.yaml \$P/conf.d/nginx.d/conf.yaml
chmod 0755 agent dsdsample parity
echo "DFLY-ENV: \$(uname -a)"
echo "DFLY-TOOLS: \$(which cc gcc clang fetch openssl 2>/dev/null)"
if cc -o offdump offdump.c 2>offdump.err; then
	./offdump | sed 's/^/OFF /'
else
	echo "OFFDUMP-CC-FAIL: \$(cat offdump.err)"
fi
sysctl -b kern.proc.all > proc.bin 2>/dev/null
echo "DFLY-PROCALL-BYTES: \$(wc -c < proc.bin)"
echo OFFCAP-B64-START
openssl base64 -in proc.bin 2>/dev/null || uuencode -m proc.bin proc.bin
echo OFFCAP-B64-END
echo "DFLY-DEVSTAT: version=\$(sysctl -n kern.devstat.version 2>/dev/null) numdevs=\$(sysctl -n kern.devstat.numdevs 2>/dev/null)"
sysctl -b kern.devstat.all > devstat.bin 2>/dev/null
echo "DFLY-DEVSTAT-BYTES: \$(wc -c < devstat.bin)"
echo DEVSTAT-B64-START
openssl base64 -in devstat.bin 2>/dev/null
echo DEVSTAT-B64-END
iostat -d 2>&1 | sed 's/^/IOSTAT /' | head -12
echo OFFCAP-DONE
if [ "\$STAGE" = capture ]; then echo CAPTURE-PASS-9X7; exit 0; fi
: > access.log
./parity serve -dir rec ours=127.0.0.1:18080 >parity.log 2>&1 &
./agent --cfgpath datadog.yaml --debug >agent.log 2>&1 &
sleep 3
cat access.seed >> access.log
./dsdsample -addr 127.0.0.1:8125 -duration 30s -tags '${RUN_TAG},${TEST_TAG}' -prefix microagent.vm.dsd
sleep 10
pkill agent; sleep 3; pkill parity; sleep 1
echo "records: \$(wc -l < rec/ours.jsonl)"
if ./parity verify -series datadog.agent.running,microagent.vm.dsd.gauge,microagent.vm.dsd.requests,microagent.vm.dsd.render.95percentile,microagent.vm.dsd.latency.avg,microagent.vm.dsd.users,system.mem.total,system.disk.total,system.load.1,system.uptime,system.io.r_s -check microagent.vm.dsd.check -event 'dsdsample up' -platform dragonfly -meta -host ${HOST_NAME} -min-procs 10 -proc-name agent -log microagent-e2e-${RUN_ID} rec/ours.jsonl; then
	echo DFLY-PASS-9X7
else
	echo DFLY-FAIL-9X7
fi
EOF

python3 -m http.server "$HTTP_PORT" --bind 0.0.0.0 --directory "$W/srv" >"$W/httpd.log" 2>&1 &
HTTP_PID=$!
sleep 1

qemu-img create -f qcow2 -b "$BASE_IMG" -F raw "$W/overlay.qcow2" >/dev/null 2>&1

# The serial driver: escape the loader to the OK prompt, switch the console to com0, boot
# multi-user, log in as root, configure the SLIRP NIC, fetch and run the workflow, and wait
# for the sentinel. Shell vars (W/HTTP_PORT/...) expand here. Every expect "$" is escaped
# as \$ and \r is a literal carriage return.
cat > "$W/drive.expect" <<EXP
set timeout 240
log_file -a $W/serial.log
proc mon {cmd} { exec sh -c "echo '\$cmd' | socat - UNIX-CONNECT:$W/mon.sock" }
proc typestr {s} {
  foreach c [split \$s ""] {
    switch -- \$c { " " {set k spc} "=" {set k equal} "\r" {set k ret} default {set k \$c} }
    mon "sendkey \$k"; after 70
  }
}
proc dbg {m} { puts "DBG: \$m"; flush stdout }
proc bail {m} { puts "\nDRIVE-FAIL: \$m"; flush stdout; catch {mon "screendump $W/fail.ppm"}; catch {exec kill [exp_pid]}; exit 1 }

spawn qemu-system-x86_64 -enable-kvm -cpu host -m 2048 -smp 2 \
  -drive file=$W/overlay.qcow2,format=qcow2,if=virtio \
  -netdev user,id=net0 -device virtio-net-pci,netdev=net0 \
  -display none -vga std -monitor unix:$W/mon.sock,server,nowait -serial stdio

# The loader menu is interactive from ~5s: option 9 escapes to the OK prompt (on the VGA
# console, so via the monitor sendkey), "set console=comconsole" moves the console to com0,
# then boot runs multi-user over serial.
after 5200; mon "sendkey 9"
after 1200; typestr "set console=comconsole\r"
after 1500; send "boot\r"
dbg "sent boot (multi-user boot over the virtio disk is slow, be patient)"
set timeout 200
expect { -re {login:} {} timeout { bail "no login prompt after boot" } }
# root logs in with no password. Send it right after the prompt, then wait on the shell
# prompt with expect (not a blind sleep) so expect keeps draining the serial pty (a long
# blind wait lets it fill and blocks the guest at the console). Then drop the login tcsh
# to /bin/sh for the workflow.
send "root\r"
set timeout 40
expect {
  -re {[Pp]assword:} { send "\r"; exp_continue }
  -re {\# }          {}
  timeout            { bail "no root shell after login" }
}
send "exec sh\r"
expect { -re {\# } {} timeout {} }
dbg "logged in, configuring network"
# SLIRP always assigns 10.0.2.15 with gateway 10.0.2.2, so a static address is deterministic.
send "ifconfig vtnet0 inet 10.0.2.15/24 up; route add default 10.0.2.2 >/dev/null 2>&1; echo NETCFG9X\r"
expect { -re {NETCFG9X} {} timeout {} }
send "fetch -o /tmp/run.sh http://10.0.2.2:${HTTP_PORT}/run.sh 2>&1; echo FETCHRC=\$?X\r"
expect {
  -re {FETCHRC=0X}    {}
  -re {FETCHRC=[1-9]} { bail "run.sh fetch failed (fetch error in serial tail)" }
  timeout             { bail "fetch timeout" }
}
dbg "fetched run.sh"
send "sh /tmp/run.sh\r"
set timeout 260
expect {
  -re {DFLY-PASS-9X7}     { dbg "verify pass" }
  -re {CAPTURE-PASS-9X7}  { dbg "capture pass" }
  -re {DFLY-FAIL-9X7}     { bail "parity verify failed in guest" }
  -re {DFLY-FETCH-9X7}    { bail "guest could not fetch binaries from the host" }
  -re {DFLY-RUN-9X7}      { bail "guest could not enter the work dir" }
  timeout                 { bail "workflow timed out" }
}
send "halt -p\r"
after 1500
catch {exec kill [exp_pid]}
exit 0
EXP

log "booting DragonFly image and driving over serial"
stop_here build
if expect -f "$W/drive.expect"; then
	# Persist the struct capture for kinfoprocdf.go / its test, from the serial log.
	grep -a '^OFF ' "$W/serial.log" 2>/dev/null | sed 's/^OFF //' | tr -d '\r' > "$CAP/offsets.txt"
	sed -n '/OFFCAP-B64-START/,/OFFCAP-B64-END/p' "$W/serial.log" 2>/dev/null \
		| grep -avE 'OFFCAP-B64' | tr -d '\r' | openssl base64 -d > "$CAP/proc.bin" 2>/dev/null
	sed -n '/DEVSTAT-B64-START/,/DEVSTAT-B64-END/p' "$W/serial.log" 2>/dev/null \
		| grep -avE 'DEVSTAT-B64' | tr -d '\r' | openssl base64 -d > "$CAP/devstat.bin" 2>/dev/null
	[ -s "$CAP/offsets.txt" ] && { echo "==> struct offsets ($CAP/offsets.txt):"; sed 's/^/    /' "$CAP/offsets.txt"; }
	[ -s "$CAP/proc.bin" ] && log "captured kern.proc.all blob to $CAP/proc.bin ($(wc -c <"$CAP/proc.bin") bytes)"
	[ -s "$CAP/devstat.bin" ] && log "captured kern.devstat.all blob to $CAP/devstat.bin ($(wc -c <"$CAP/devstat.bin") bytes)"
	echo "==> DRAGONFLY VM E2E (fake intake) PASS"; exit 0
fi
echo "==> DRAGONFLY VM E2E (fake intake) FAIL"
echo "--- serial tail ---"; tail -40 "$W/serial.log" 2>/dev/null | tr -d '\r' | sed 's/\x08//g'
exit 1
