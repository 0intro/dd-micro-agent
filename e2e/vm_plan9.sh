#!/usr/bin/env bash
#
# Full end-to-end test on Plan 9. It clones 0intro/9legacy, installs it with the
# repo's own boot/getbin (plan9port 9p, no sudo), and boots it under QEMU with a
# virtio-9p root + an ELF kernel via the repo's boot/qemu -386, driven like the repo's
# boot/test CI harness. It runs the cross-built (GOOS=plan9 GOARCH=386) agent with
# metrics + logs + host metadata, starts the real Plan 9 ip/httpd for Common Log Format
# access logs, and verifies via the `pup` CLI that the Plan 9 host metrics reached
# Datadog, the core ones (/dev/sysstat, /dev/swap, /dev/time) plus the newer
# /net/tcp/stats, /proc, and /dev/sysstat-counter collectors, that the host appears
# (with its plan9 platform/gohai), that the logs arrived, that the "Plan 9 system
# logs" pipeline classifies an injected kernel-panic line into @plan9.event, and that
# the venti reporter's disk/server metrics arrive (scraped from a faked /storage page,
# since the VM has no real venti), and that a Go program on Plan 9 can profile itself
# (heap + goroutine, no CPU) and upload through the agent's profiling proxy.
#
# It clones github.com/0intro/9legacy (override with PLAN9_REPO), which carries the
# virtio-9p + ELF + sudo-free boot/getbin machinery, and caches the checkout under
# PLAN9_CACHE so later runs reuse it.
#
# The virtio-9p root IS the checkout directory, so the agent binary, config, the rc
# workload, and the CA bundle are staged by writing into the checkout on the host (no
# scp/hget). The guest's agent.log reads back on the host the same way.
#
# Two modes: DD_API_KEY set posts to real Datadog and verifies with pup (manual),
# unset posts to a local fake intake (e2e/parity) in the guest and verifies the
# recording with `parity verify` (automated CI mode, no keys, no pup).
#
#   DD_API_KEY / DD_APP_KEY   set for real-Datadog + pup, unset for fake intake
#   DD_SITE                   defaults to datadoghq.eu (real mode only)
#   PLAN9_REPO                clone source (default https://github.com/0intro/9legacy)
#   PLAN9_CACHE               clone/install dir (default /tmp/plan9-9legacy)
#   STOP_AFTER                debug knob: provision | verify (default)
#
# Needs KVM, qemu-system-x86_64, expect, git, plan9port (9660srv + 9p, for boot/getbin),
# curl, bunzip2, and outbound network (run with the sandbox OFF). pup + jq are needed
# only for the real-Datadog verify.
#
set -uo pipefail

FAKE=0
[ -z "${DD_API_KEY:-}" ] && FAKE=1
if [ "$FAKE" = 0 ]; then
	: "${DD_APP_KEY:?set DD_APP_KEY (or unset DD_API_KEY to use the fake-intake mode)}"
	export DD_API_KEY DD_APP_KEY
fi
export DD_SITE="${DD_SITE:-datadoghq.eu}"
STOP_AFTER="${STOP_AFTER:-verify}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CACHE="${PLAN9_CACHE:-/tmp/plan9-9legacy}"
CKOUT="$CACHE/9legacy"
# Clone github.com/0intro/9legacy (the virtio-9p+ELF + sudo-free boot/getbin machinery
# lives there). Override PLAN9_REPO to point at a local working copy instead.
REPO="${PLAN9_REPO:-https://github.com/0intro/9legacy}"
W=/tmp/microagent-vm-plan9
DD=/usr/glenda/dd          # guest path (on the virtio-9p root) for our files
GUESTDD="$CKOUT$DD"        # the same directory as seen on the host
SERIAL="$W/serial.log"

RUN_ID="$(date +%s)$RANDOM"
HOST_NAME="microagent-plan9-e2e-${RUN_ID}"
RUN_TAG="e2e_run:${RUN_ID}"
TEST_TAG="test:microagent-e2e"

# ddpup runs pup with an isolated HOME+config so it authenticates with the provided
# keys (not a stored OAuth session) and reads the same org the agent writes to.
PUP_HOME="$W/pup"
ddpup() { HOME="$PUP_HOME" XDG_CONFIG_HOME="$PUP_HOME" pup "$@"; }
log() { echo "==> $*"; }

cleanup() {
	log "cleanup"
	# the qemu serving this checkout over virtio-9p (its -fsdev arg names the path)
	pkill -f "path=$CKOUT,security_model" 2>/dev/null
	# keep $W (serial.log) and the cached checkout for post-mortem. Next run cleans $W
}
trap cleanup EXIT

deps="qemu-system-x86_64 expect git 9660srv 9p curl bunzip2"
[ "$FAKE" = 0 ] && deps="$deps pup jq" # pup + jq only for the real-Datadog verify
for c in $deps; do
	command -v "$c" >/dev/null || { echo "$c missing (9660srv/9p come from plan9port)"; exit 1; }
done
[ -w /dev/kvm ] || echo "warning: /dev/kvm not writable; QEMU will be slow"
pkill -f "path=$CKOUT,security_model" 2>/dev/null && sleep 1
rm -rf "$W"; mkdir -p "$PUP_HOME"
log "run_id=$RUN_ID host=$HOST_NAME site=$DD_SITE"

log "building static agent + dogstatsd sample (plan9/386)"
( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod GOOS=plan9 GOARCH=386 CGO_ENABLED=0 \
	go build -tags netgo -o "$W/agent" ./cmd/agent ) ||
	{ echo "build failed"; exit 1; }
( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod GOOS=plan9 GOARCH=386 CGO_ENABLED=0 \
	go build -tags netgo -o "$W/dsdsample" ./e2e/dsdsample ) ||
	{ echo "dsdsample build failed"; exit 1; }
if [ "$FAKE" = 1 ]; then
	# the recorder runs in the guest (plan9/386), the verifier on the host
	( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod GOOS=plan9 GOARCH=386 CGO_ENABLED=0 \
		go build -tags netgo -o "$W/parity" ./e2e/parity ) || { echo "parity (plan9) build failed"; exit 1; }
	( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0 \
		go build -o "$W/parity-host" ./e2e/parity ) || { echo "parity (host) build failed"; exit 1; }
	# The profiler exercise is the package's own test binary cross-built for Plan 9.
	# It collects a real profile on the guest and uploads it through the agent's proxy.
	( cd "$ROOT" && GOTOOLCHAIN=local GOFLAGS=-mod=mod GOOS=plan9 GOARCH=386 CGO_ENABLED=0 \
		go test -c -tags netgo -o "$W/profiler.test" ./profiler ) || { echo "profiler.test (plan9) build failed"; exit 1; }
fi

if [ ! -f "$CKOUT/386/9pcf.elf" ]; then
	log "cloning + installing 9legacy into $CKOUT (first run downloads the CD)"
	mkdir -p "$CACHE"
	[ -d "$CKOUT/.git" ] || git clone --depth 1 "$REPO" "$CKOUT" || { echo "clone failed"; exit 1; }
	# boot/getbin defaults to plan9port's 9p (no sudo) when 9p is present.
	( cd "$CKOUT" && ./boot/mkdirs && ./boot/getbin ) ||
		{ echo "boot/getbin failed (needs plan9port's 9660srv + 9p)"; exit 1; }
	[ -f "$CKOUT/386/9pcf.elf" ] || { echo "386/9pcf.elf missing after getbin"; exit 1; }
else
	log "using cached 9legacy checkout at $CKOUT"
fi

# The checkout is the guest's root over virtio-9p, so writing here == writing in the
# guest. No hget / host HTTP server, and ca.pem is written host-side (no guest perms).
log "staging agent, config, CA bundle, and rc workload into the checkout"
mkdir -p "$GUESTDD/confd" "$GUESTDD/run" "$CKOUT/usr/glenda/bin/rc" "$CKOUT/sys/lib/tls"
install -m0755 "$W/agent" "$GUESTDD/agent"
install -m0755 "$W/dsdsample" "$GUESTDD/dsdsample"

if [ "$FAKE" = 1 ]; then
	install -m0755 "$W/parity" "$GUESTDD/parity"
	install -m0755 "$W/profiler.test" "$GUESTDD/profiler.test"
	# Fake-intake config: post metrics, logs, metadata, processes, and the venti scrape
	# results to the in-guest recorder over plain HTTP (no TLS, no ca.pem dance).
	cat > "$GUESTDD/datadog.yaml" <<EOF
api_key: dummy
dd_url: http://127.0.0.1:18080
hostname: ${HOST_NAME}
tags:
  - ${TEST_TAG}
  - ${RUN_TAG}
logs_enabled: true
enable_metadata_collection: true
logs_config:
  batch_wait: 1
  logs_dd_url: http://127.0.0.1:18080
run_path: ${DD}/run
confd_path: ${DD}/confd
venti_url: http://127.0.0.1:9999
process_config:
  process_dd_url: http://127.0.0.1:18080
  process_collection:
    enabled: true
apm_config:
  enabled: true
  receiver_host: 127.0.0.1
  receiver_port: 18126
  profiling_dd_url: http://127.0.0.1:18080/api/v2/profile
EOF
else
	cat > "$GUESTDD/datadog.yaml" <<EOF
api_key: ${DD_API_KEY}
site: ${DD_SITE}
hostname: ${HOST_NAME}
tags:
  - ${TEST_TAG}
  - ${RUN_TAG}
logs_enabled: true
enable_metadata_collection: true
logs_config:
  batch_wait: 1
run_path: ${DD}/run
confd_path: ${DD}/confd
venti_url: http://127.0.0.1:9999
process_config:
  process_collection:
    enabled: true
EOF
fi

# Glob sources tail every Plan 9 system log (dns, auth, cs, mail, …) under one
# service, plus the httpd subdir (CLF) under another. /sys/log/* skips the httpd
# directory (non-regular). /sys/log/httpd/* picks up clf and friends.
cat > "$GUESTDD/confd/plan9.yaml" <<EOF
logs:
  - type: file
    path: /sys/log/*
    service: plan9
    source: plan9
  - type: file
    path: /sys/log/httpd/*
    service: httpd
    source: nginx
  - type: file
    path: ${DD}/kmesg.log
    service: plan9-kernel
    source: plan9
EOF

# CA bundle for Go's crypto/x509 (reads /sys/lib/tls/ca.pem on plan9). The clone ships
# one. Overwrite it with the host's current bundle so datadoghq.eu's chain validates.
CA_SRC=""
for c in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/cert.pem; do
	[ -r "$c" ] && { CA_SRC="$c"; break; }
done
if [ -n "$CA_SRC" ]; then cp "$CA_SRC" "$CKOUT/sys/lib/tls/ca.pem"; else echo "warning: no host CA bundle; keeping the clone's ca.pem"; fi

# In fake mode, start the in-guest recorder before the agent (rc syntax) so it captures
# the startup metadata. Expanded host-side into RECORDER_START, empty in real mode.
RECORDER_START=""
[ "$FAKE" = 1 ] && RECORDER_START="${DD}/parity serve -dir ${DD}/rec 'ours=127.0.0.1:18080' >${DD}/parity.log >[2=1] & sleep 1"

# The in-guest rc workload, run by the boot/test-style driver below. The boot/qemu -386
# terminal brings up slirp networking by itself (10.0.2.15). We ensure a resolver, run
# the agent, drive httpd for CLF, then exit 0 iff the agent reported a delivered batch.
cat > "$CKOUT/usr/glenda/bin/rc/ddtest" <<EOF
#!/bin/rc
# A multiboot virtio-9p boot leaves /net unconfigured. Bring up slirp networking
# (ip 10.0.2.15, gw 10.0.2.2, dns 10.0.2.3) and the connection/dns servers.
bind -a '#I' /net >[2]/dev/null
bind -a '#l0' /net >[2]/dev/null
ip/ipconfig -g 10.0.2.2 ether /net/ether0 10.0.2.15 255.255.255.0 >[2]/dev/null
ip/ipconfig loopback /dev/null 127.0.0.1 >[2]/dev/null
ndb/cs >[2]/dev/null
ndb/dns -r >[2]/dev/null &
sleep 1
# Fake a venti /storage endpoint (this VM has no real venti): aux/listen1 forks a cat
# per connection, returning the canned HTTP response for any path. The agent's /storage
# scrape parses it. Its /graph requests get the same body and skip gracefully.
{
	echo 'HTTP/1.0 200 OK'
	echo 'Content-Type: text/plain'
	echo 'Connection: close'
	echo ''
	echo 'index=main'
	echo 'total arenas=4 active=3'
	echo 'total space=10,000,000,000 used=4,000,000,000'
	echo 'clumps=12,345 compressed clumps=10,000 data=8,000,000,000 compressed data=3,500,000,000'
} >${DD}/venti-storage.http
aux/listen1 -t 'tcp!*!9999' cat ${DD}/venti-storage.http >[2]/dev/null &
sleep 1
${RECORDER_START}
${DD}/agent -cfgpath ${DD}/datadog.yaml -debug >${DD}/agent.log >[2=1] &
sleep 1
# Drive a DogStatsD workload at the agent's UDP listener (127.0.0.1!8125, loopback is
# configured above) so the metrics pipeline runs on Plan 9 too: gauge, counter,
# histogram, timing, set, plus a service check and event. Backgrounded so it overlaps
# the workload's existing sleep. The agent's 15s flush ships it.
${DD}/dsdsample -addr 127.0.0.1:8125 -duration 25s -tags '${RUN_TAG},${TEST_TAG}' -prefix microagent.vm.dsd >[2]/dev/null &
# Profile this Plan 9 Go runtime through the agent's proxy. The profiler package's own
# test binary collects heap + goroutine profiles (CPU is empty on Plan 9, so the library
# drops it) and uploads them to the proxy on 18126, which forwards to the fake intake.
# rc unifies variables and the environment, so the child sees PROFILE_PROXY_URL.
sleep 2
PROFILE_PROXY_URL=http://127.0.0.1:18126/profiling/v1/input
${DD}/profiler.test -test.run TestUpload >${DD}/profiler.log >[2=1]
# Mirror the kernel message buffer into a log file the agent tails (an explicit
# source). /dev/kmesg is a sliding ring (not tailable directly), so seed from the ring
# (boot history), then stream the live /dev/kprint queue, a separate exclusive reader,
# so it doesn't block /dev/kmesg readers. We write the agent's own (writable) dir
# because /sys/log is sys-owned (glenda can't create there). A real boot service would
# write /sys/log/kmesg, which the /sys/log/* glob would then pick up.
cat /dev/kmesg >>${DD}/kmesg.log >[2]/dev/null
cat /dev/kprint >>${DD}/kmesg.log >[2]/dev/null &
# Start the real Plan 9 web server and hit it so it writes /sys/log/httpd/clf.
# httpd announces tcp!*!http itself, so it's run directly, not under aux/listen1,
# which would also bind port 80 ("address in use"). Its stderr -> httpd.log.
ip/httpd/httpd >${DD}/httpd.log >[2=1] &
sleep 1
hget 'http://127.0.0.1/microagent-e2e-${RUN_ID}' >/dev/null >[2]/dev/null
# Guarantee a Common Log Format access line (with the run token) so the agent
# (already tailing /sys/log/httpd/clf) ships it. Mirrors the FreeBSD e2e, which
# synthesizes the nginx access log rather than depending on a live server.
echo '127.0.0.1 - - [19/Jun/2026:12:00:00 +0000] "GET /microagent-e2e-${RUN_ID} HTTP/1.0" 200 42' >>/sys/log/httpd/clf
# Exercise the "Plan 9 system logs" pipeline end-to-end: a synthetic kernel-panic
# syslog line shipped via kmesg.log (source:plan9) should classify to
# @plan9.event:kernel_panic at ingestion. (A real panic lands in /dev/kmesg too.)
echo 'gnot Jun 19 12:00:00 panic: microagent-e2e-${RUN_ID} consecutive faults' >>${DD}/kmesg.log
sleep 45
# Diagnostic: capture the real /net structure and stat-file formats so the host can
# confirm the new network collectors read the right paths with the labels we expect.
{
	echo '### ls /net'; ls /net
	echo '### ls /net/tcp'; ls /net/tcp
	echo '### ls /net/ipifc'; ls /net/ipifc
	echo '### /net/ipifc/stats (IP)'; cat /net/ipifc/stats
	echo '### /net/icmp/stats'; cat /net/icmp/stats
	echo '### /net/udp/stats'; cat /net/udp/stats
	echo '### /net/tcp/0/status'; cat /net/tcp/0/status
	echo '### /net/ipifc/0/status'; cat /net/ipifc/0/status
	echo '### /net/iproute'; cat /net/iproute
	echo '### /net/arp'; cat /net/arp
	echo '### /dev/sysstat'; cat /dev/sysstat
	echo '### /net/ether0/ifstats'; cat /net/ether0/ifstats
	echo '### ls /srv'; ls /srv
	echo '### ls /mnt'; ls /mnt
} >${DD}/netdump.txt >[2=1]
# Ground truth for the Live Processes check: the VM's own process table (read back
# host-side over virtio-9p), captured right after the agent's last collect. ps -a
# gives user, pid, times, size, state, and the full command per process.
ps -a >${DD}/ps.txt >[2]/dev/null
# Ground truth for the process-memory share-count dedup: dump every proc's status and
# segment. A Plan 9 program is many rfork(RFMEM) procs sharing one address space (each
# reports the SAME status memory), so the host checks the reported (deduped) metrics sit
# well below the naive status-size sum and that shared Data/Bss segments (ref>1) exist.
# Plan 9 status files have no trailing newline, so emit one per line for the host to sum.
{ for(f in /proc/[0-9]*/status){ cat \$f; echo } } >${DD}/proc-status.txt >[2]/dev/null
cat /proc/[0-9]*/segment >${DD}/proc-seg.txt >[2]/dev/null
# Require a log batch, a process payload, and a forwarded profile to have been
# delivered (2xx) for the run to count as OK. Each Reporter logs its line on success.
grep 'log batch sent' ${DD}/agent.log >/dev/null && grep 'process payload sent' ${DD}/agent.log >/dev/null && grep 'profile forwarded' ${DD}/agent.log >/dev/null
EOF
chmod 0755 "$CKOUT/usr/glenda/bin/rc/ddtest"

# Drive the guest exactly like the repo's boot/test: expect spawns boot/qemu -386
# (virtio-9p root + ELF kernel, serial console), waits for the rc prompt, slow-sends
# one command, and waits for a DD-OK / DD-FAIL sentinel. The ^ in "DD^-OK" is rc
# concatenation, so the echoed command shows DD^-OK while the real output is DD-OK
# (we match only output).
log "booting plan9-contrib (virtio-9p root + ELF kernel) and running the workload"
: >"$SERIAL"
SERIAL="$SERIAL" expect -f - "$CKOUT/boot/qemu" -386 <<'EXPECT'
log_file -a $env(SERIAL)
set timeout 300
spawn -noecho {*}$argv
set send_slow {1 0.02}
proc bail {code} { catch {exec kill [exp_pid]}; exit $code }
proc die {msg} { puts stderr "\nvm_plan9: $msg"; bail 1 }
proc atprompt {} {
	expect {
		-re {% $} {}
		timeout { die "no shell prompt" }
		eof { die "qemu exited" }
	}
}
proc waitready {} {
	global timeout
	set save $timeout
	set timeout 2
	for {set i 0} {$i < 90} {incr i} {
		send "\r"
		expect {
			-re {% $} { set timeout $save; return }
			timeout {}
			eof { die "qemu exited" }
		}
	}
	die "console rc never became ready"
}
atprompt
waitready
send -s -- "rc /usr/glenda/bin/rc/ddtest && echo DD^-OK || echo DD^-FAIL"
send "\r"
set timeout 300
expect {
	"DD-OK" {}
	"DD-FAIL" { die "ddtest reported no log batch (see serial log + agent.log)" }
	timeout { die "ddtest timed out" }
	eof { die "qemu exited" }
}
atprompt
bail 0
EXPECT
rc=$?

if [ "$rc" -ne 0 ]; then
	echo "  FAIL  guest workload failed (rc=$rc); serial tail:"; tail -40 "$SERIAL" 2>/dev/null
	echo "  agent.log tail:"; tail -20 "$GUESTDD/agent.log" 2>/dev/null
	exit 1
fi
log "guest workload OK (agent ran, log batch delivered)"
log "agent.log tail:"; tail -15 "$GUESTDD/agent.log" 2>/dev/null
[ "$STOP_AFTER" = provision ] && { log "STOP_AFTER=provision reached (serial log at $SERIAL)"; exit 0; }

# Fake-intake mode: assert the recording (written by the in-guest recorder onto the
# virtio-9p root, read back here) carries the DogStatsD workload, the Plan 9 host
# metrics, host metadata (platform plan9), the process list, and the unique log line.
if [ "$FAKE" = 1 ]; then
	REC="$GUESTDD/rec/ours.jsonl"
	log "records: $(wc -l <"$REC" 2>/dev/null)"
	log "verifying the fake-intake recording (parity verify)"
	if "$W/parity-host" verify \
		-series datadog.agent.running,microagent.vm.dsd.gauge,microagent.vm.dsd.requests,microagent.vm.dsd.render.95percentile,microagent.vm.dsd.latency.avg,microagent.vm.dsd.users,system.mem.total,system.load.1 \
		-check microagent.vm.dsd.check -event 'dsdsample up' \
		-platform plan9 -meta -host "${HOST_NAME}" \
		-min-procs 10 -proc-name agent \
		-log "microagent-e2e-${RUN_ID}" \
		-profile -profile-family go -profile-attach heap.pprof \
		"$REC"; then
		echo "==> PLAN9 VM E2E (fake intake) PASS"; exit 0
	fi
	echo "==> PLAN9 VM E2E (fake intake) FAIL"
	echo "--- agent.log tail ---"; tail -20 "$GUESTDD/agent.log" 2>/dev/null
	exit 1
fi

metric_present() { # $1 = metric name
	ddpup metrics query --query="avg:$1{${RUN_TAG}}" --from=15m --output json 2>/dev/null |
		jq -e '.data.series | length > 0' >/dev/null
}
metric_is_42() { # the dogstatsd sample's gauge, value-checked end-to-end
	ddpup metrics query --query="avg:microagent.vm.dsd.gauge{${RUN_TAG}}" --from=15m --output json 2>/dev/null |
		jq -e '[.data.series[]?.pointlist[]?[1] | select(. != null)] | any(. == 42)' >/dev/null
}
metric_has_tag() { # $1 = metric name, $2 = tag key. Proves the metric is split by it
	ddpup metrics query --query="avg:$1{${RUN_TAG}} by {$2}" --from=15m --output json 2>/dev/null |
		jq -e --arg k "$2:" '[.data.series[]?.scope // empty | select(test($k))] | length > 0' >/dev/null
}
host_present() {
	# The host appears in the Infrastructure List carrying our run tag. We don't assert
	# .meta.agent_version: the v5 .meta block (and the host-detail page) don't populate
	# on this org (Plan 9 isn't a recognized Agent platform) even though the metadata
	# is delivered (see the agent-2xx check below). This matches e2e.sh's tag-based host
	# check, which exists precisely because that readback is gated on some orgs.
	ddpup infrastructure hosts list --filter="host:${HOST_NAME}" --output json 2>/dev/null |
		jq -e --arg t "$RUN_TAG" '(.data.host_list[0].tags_by_source.Datadog | index($t)) != null' >/dev/null
}
logs_searchable() {
	ddpup logs search --query="service:httpd microagent-e2e-${RUN_ID}" --from=20m --limit=10 --output json 2>/dev/null |
		jq -e '.data | length > 0' >/dev/null
}
event_classified() { # $1 = expected @plan9.event value for the injected kmesg panic line
	ddpup logs search --query="source:plan9 @plan9.event:$1 microagent-e2e-${RUN_ID}" --from=20m --limit=5 --output json 2>/dev/null |
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
# DogStatsD pipeline (over loopback): the sample service's gauge value is exact, the
# counter ships as a rate, the histogram expands to .avg/.95percentile/…, the set
# ships a distinct-member count, proving the metrics path works on Plan 9 too.
wait_for "dogstatsd gauge = 42"                      300 metric_is_42                                         || pass=1
wait_for "dogstatsd counter present"                 300 metric_present microagent.vm.dsd.requests            || pass=1
wait_for "dogstatsd histogram .95percentile present" 300 metric_present microagent.vm.dsd.render.95percentile || pass=1
wait_for "dogstatsd set distinct-count present"      300 metric_present microagent.vm.dsd.users               || pass=1
wait_for "host metric system.mem.total present" 300 metric_present system.mem.total || pass=1
wait_for "host metric system.cpu.idle present"  300 metric_present system.cpu.idle  || pass=1
wait_for "host metric system.uptime present"    300 metric_present system.uptime    || pass=1
# New collectors: CPU activity (rate, /dev/sysstat counters), TCP (/net/tcp/stats),
# processes (/proc/*/status), and network errors (/net/etherN/stats). Each emits a
# series even when the value is zero, so its presence proves the collector read and
# parsed the real kernel file on a running Plan 9.
wait_for "metric system.cpu.context_switches present" 300 metric_present system.cpu.context_switches || pass=1
wait_for "metric system.cpu.tlb_purges present"       300 metric_present system.cpu.tlb_purges       || pass=1
# CPU temperature is best-effort: /dev/cputemp reads thermal MSRs that QEMU may not expose
# (it then returns the "-1 -1 unsupported" sentinel and the collector emits nothing).
if metric_present system.cpu.temp; then echo "  ok    metric system.cpu.temp present"; else echo "  note  system.cpu.temp absent (QEMU likely has no thermal sensor)"; fi
wait_for "metric system.net.tcp.current_established present" 300 metric_present system.net.tcp.current_established || pass=1
wait_for "metric system.proc.count present"            300 metric_present system.proc.count            || pass=1
wait_for "metric system.net.errors.count present"      300 metric_present system.net.errors.count      || pass=1
# Deeper network collectors (this change): per-connection TCP states (/net/tcp/*/status),
# per-protocol IP/ICMP/UDP stats (/net/{ipifc,icmp,udp}/stats), and the route/arp tables.
# Each emits a continuous (zero-filled) series, so presence proves the path + parse.
wait_for "metric system.net.tcp.time_wait present"     300 metric_present system.net.tcp.time_wait     || pass=1
wait_for "metric system.net.ip.in_receives present"    300 metric_present system.net.ip.in_receives    || pass=1
wait_for "metric system.net.udp.in_datagrams present"  300 metric_present system.net.udp.in_datagrams  || pass=1
wait_for "metric system.net.icmp.in_msgs present"      300 metric_present system.net.icmp.in_msgs      || pass=1
wait_for "metric system.net.iproute.count present"     300 metric_present system.net.iproute.count     || pass=1
wait_for "metric system.net.arp.entries present"       300 metric_present system.net.arp.entries       || pass=1
# Service/mount counts (/srv, /mnt entry counts), present on any booted Plan 9.
wait_for "metric system.srv.count present"             300 metric_present system.srv.count             || pass=1
wait_for "metric system.mnt.count present"             300 metric_present system.mnt.count             || pass=1
# ifstats (per-NIC driver counters) and clock (timesync drift) are best-effort: the VM's
# virtio-net exposes no "Label: value" ifstats counters, and the stock boot runs no timesync.
if metric_present system.clock.frequency; then echo "  ok    metric system.clock.frequency present"; else echo "  note  system.clock.* absent (no aux/timesync running)"; fi
# Per-core CPU and per-state processes now carry a cpu:/state: tag.
wait_for "metric system.cpu.idle split by cpu"         300 metric_has_tag system.cpu.idle cpu          || pass=1
wait_for "metric system.proc.count split by state"     300 metric_has_tag system.proc.count state      || pass=1
if metric_present system.mem.kernel.malloc; then
	echo "  ok    metric system.mem.kernel.malloc present"
else
	echo "  note  system.mem.kernel.malloc absent (this kernel may not export the pool)"
fi
# Venti: the agent scrapes the faked /storage endpoint and ships disk + server metrics.
wait_for "venti metric venti.clumps.total present"     300 metric_present venti.clumps.total || pass=1
wait_for "venti disk metric system.disk.total present" 300 metric_present system.disk.total  || pass=1
wait_for "host in Infrastructure List w/ run tag" 300 host_present || pass=1
echo "  ok    logs delivered (agent reported 'log batch sent')" # proven by DD-OK above
# Service checks and events from the dogstatsd sample: delivery is the assertion (the
# aggregator logs a 2xx), proving the _sc/_e parse + check_run / intake forwarding.
if grep -q 'service checks sent' "$GUESTDD/agent.log" 2>/dev/null; then echo "  ok    service check delivered (agent 2xx)"; else echo "  FAIL  service check not delivered"; pass=1; fi
if grep -q 'events sent' "$GUESTDD/agent.log" 2>/dev/null; then echo "  ok    event delivered (agent 2xx)"; else echo "  FAIL  event not delivered"; pass=1; fi

# Metadata (v5 + modern inventory) is delivered with 2xx even though this org doesn't
# surface it on the host-detail page (Plan 9 is an unrecognized Agent platform).
if grep -q 'inventory host metadata sent' "$GUESTDD/agent.log" 2>/dev/null; then
	echo "  ok    host + inventory metadata delivered (agent 2xx)"
else
	echo "  FAIL  metadata not delivered by agent"; pass=1
fi
# Kernel-log capture: the /dev/kprint copier feeds kmesg.log, tailed as a source.
if grep -q 'kmesg.log' "$GUESTDD/agent.log" 2>/dev/null; then
	echo "  ok    kernel log (kmesg.log) tailed (/dev/kprint copier)"
else
	echo "  note  kernel log not tailed"
fi
# Venti reporter: confirm the agent scraped the faked venti and sent its metrics.
if grep -q 'venti metrics sent' "$GUESTDD/agent.log" 2>/dev/null; then
	echo "  ok    venti reporter scraped /storage and sent metrics"
else
	echo "  FAIL  venti reporter did not send (check the fake venti / venti_url)"; pass=1
fi

# Live Processes (process intake)
# The agent posts the FULL Plan 9 process list (CollectorProc, ~${GT_PROCS} procs)
# to process.<site> (the 2xx is gated into DD-OK above) and the real process table
# (init/cs/fs/httpd/factotum/… plus the agent's own churning kprocs) renders in
# Live Processes. The catch that took real-product verification to find: the backend
# silently DROPS any process reporting zero threads, so until the collector set
# Threads=1 (correct: a Plan 9 proc is one thread), the UI showed a command-count
# header over an EMPTY table and only ~14 churning agent kprocs surfaced. We assert
# the count is SUBSTANTIAL (not just "present") so that regression can't return.
#
# The v2 /processes endpoint returns a GLOBAL snapshot whose server-side host filter
# is unreliable, so we fetch the global list and filter to our host client-side. The
# host: filter itself is exercised through pup below (which does filter correctly).
PROC_JSON="$W/processes.json"
GT_PROCS=$(grep -c . "$GUESTDD/ps.txt" 2>/dev/null || echo 0)   # what the VM's ps -a saw
fetch_procs() {
	curl -s -m 30 -H "DD-API-KEY: ${DD_API_KEY}" -H "DD-APPLICATION-KEY: ${DD_APP_KEY}" \
		"https://api.${DD_SITE}/api/v2/processes?page%5Blimit%5D=1000" >"$PROC_JSON" 2>/dev/null
}
host_proc_count() { fetch_procs; jq --arg h "$HOST_NAME" '[.data[]?|select(.attributes.host==$h)]|length' "$PROC_JSON" 2>/dev/null; }
# With Threads=1 the real list surfaces (~38 of ~50). A regression that breaks
# process identity (unstable create time) or drops the thread count collapses this
# to a handful of churning agent kprocs, which a ≥20 floor catches.
procs_present() { local n; n=$(host_proc_count); [ "${n:-0}" -ge 20 ]; }
has_cmd() { jq -e --arg h "$HOST_NAME" --arg c "$1" 'any(.data[]?; .attributes.host==$h and (.attributes.tags|index("command:"+$c)))' "$PROC_JSON" >/dev/null 2>&1; }
pup_proc_n()   { ddpup --no-agent processes list --tags "host:$1" --page-limit 1000 --output json 2>/dev/null | jq '[.data[]?]|length' 2>/dev/null; }
pup_all_match() { ddpup --no-agent processes list --tags "host:$HOST_NAME" --page-limit 1000 --output json 2>/dev/null | jq -e --arg h "$HOST_NAME" 'all(.data[]?; .attributes.host==$h)' >/dev/null 2>&1; }

log "verifying Live Processes (process intake) via the v2 processes API"
wait_for "process list present in Live Processes (>=20 procs)" 360 procs_present || pass=1
N=$(host_proc_count 2>/dev/null || echo 0)
echo "  ..    Live Processes shows ${N} of ${GT_PROCS} sent (rest are 0-memory kernel threads)"
# The heavyweight process (the agent itself, tens of MB) reliably surfaces, command-tagged.
if has_cmd agent; then echo "  ok    process 'agent' present (command-tagged)"; else echo "  FAIL  process 'agent' missing from list"; pass=1; fi
# Information: the processes carry their Plan 9 identity (user + OS).
if jq -e --arg h "$HOST_NAME" 'any(.data[]?; .attributes.host==$h and (.attributes.tags|index("user:glenda")) and (.attributes.tags|index("os_name:plan9")))' "$PROC_JSON" >/dev/null 2>&1; then
	echo "  ok    process info present (user:glenda, os_name:plan9)"
else echo "  FAIL  process info tags (user/os) missing"; pass=1; fi
# Create time: start must be the real (TReal-derived) time, not the 1970 epoch.
if jq -e --arg h "$HOST_NAME" 'any(.data[]?; .attributes.host==$h and ((.attributes.start|startswith("1970"))|not))' "$PROC_JSON" >/dev/null 2>&1; then
	echo "  ok    process start time populated (real, not the 1970 epoch)"
else echo "  FAIL  process start times are the 1970 epoch (createTime unset)"; pass=1; fi
# host: filter (the explicit requirement) via pup: this host returns only its own
# processes, and a non-existent host returns none.
RN=$(pup_proc_n "$HOST_NAME"); BN=$(pup_proc_n "${HOST_NAME}-does-not-exist")
if [ "${RN:-0}" -gt 0 ] && pup_all_match; then echo "  ok    host: filter returns only this host's processes (${RN})"; else echo "  FAIL  host: filter positive case (${RN:-0} returned)"; pass=1; fi
if [ "${BN:-0}" = 0 ]; then echo "  ok    host: filter excludes other hosts (bogus host -> 0)"; else echo "  FAIL  host: filter negative case (bogus -> ${BN})"; pass=1; fi

# process memory: address-space share-count dedup
# A Plan 9 program is many rfork(RFMEM) procs sharing one address space, and each reports
# the SAME status memory (the shared Text+Data+Bss image), so summing it by program (what
# "top process by memory" did) over-counts an N-thread program N×. The collector divides
# each proc's status size by the Data/Bss segment refcount (the share count), counting the
# image once. Ground-truth it against the VM's real /proc (dumped by the workload): the
# reported (deduped) totals must sit well below the naive status-size sum, and shared
# Data/Bss segments (ref>1) must actually exist.
wait_for "metric system.proc.memory.total present" 300 metric_present system.proc.memory.total || pass=1
SEGDUMP="$GUESTDD/proc-seg.txt"; STATDUMP="$GUESTDD/proc-status.txt"
if [ -s "$SEGDUMP" ] && [ -s "$STATDUMP" ]; then
	# Each /proc/N/status record is 12 fields with memory (kB) at field 10 (cf.
	# parseProcStatus's f[len-3]). Sum field 10 of every record, robust to whether the
	# dump is one-per-line or (if newlines were lost) all records run together.
	STATUS_KB=$(awk '{for(i=10;i<=NF;i+=12) s+=$i} END{printf "%.0f", s+0}' "$STATDUMP")
	# The share-count divisor is a Data/Bss segment's refcount. >1 means a shared image.
	if awk '($1=="Data"||$1=="Bss") && $NF+0>1{f=1} END{exit !f}' "$SEGDUMP"; then
		echo "  ok    /proc/N/segment shows shared Data/Bss (ref>1), the share-count divisor"
	else
		echo "  FAIL  no shared Data/Bss segment in dump (share-count signal missing)"; pass=1
	fi
	TOTAL_MB=$(ddpup metrics query --query="avg:system.proc.memory.total{${RUN_TAG}}" --from=15m --output json 2>/dev/null | jq -r '.data.series[0].pointlist[-1][1] // empty')
	echo "  ..    proc memory total: naive status-sum=$((STATUS_KB/1024))MB  reported (deduped)=${TOTAL_MB:-?}MB"
	if [ -n "$TOTAL_MB" ] && awk "BEGIN{exit !(${TOTAL_MB}>0 && ${TOTAL_MB}*1024 < ${STATUS_KB})}"; then
		echo "  ok    system.proc.memory.total is deduped (below the naive status-size sum)"
	else
		echo "  FAIL  proc memory total not deduped (reported=${TOTAL_MB:-?}MB, naive=$((STATUS_KB/1024))MB)"; pass=1
	fi
	# Targeted: the agent itself (many rfork'd threads), the headline of the widget.
	AGENT_KB=$(awk '{for(i=1;i+11<=NF;i+=12) if($i=="agent") a+=$(i+9)} END{printf "%.0f", a+0}' "$STATDUMP")
	AGENT_MB=$(ddpup metrics query --query="avg:system.proc.memory{${RUN_TAG},proc:agent}" --from=15m --output json 2>/dev/null | jq -r '.data.series[0].pointlist[-1][1] // empty')
	if [ "${AGENT_KB:-0}" -gt 0 ] && [ -n "$AGENT_MB" ]; then
		echo "  ..    agent program: naive status-sum=$((AGENT_KB/1024))MB  reported=${AGENT_MB}MB"
		if awk "BEGIN{exit !(${AGENT_MB}>0 && ${AGENT_MB}*1024 < ${AGENT_KB})}"; then
			echo "  ok    system.proc.memory{proc:agent} counts the shared image once (below naive sum)"
		else
			echo "  FAIL  proc:agent memory not deduped (reported=${AGENT_MB}MB, naive=$((AGENT_KB/1024))MB)"; pass=1
		fi
	else
		echo "  note  agent program memory not isolated ('agent' rows kB=${AGENT_KB:-0}, metric=${AGENT_MB:-none}), skipped"
	fi
else
	echo "  FAIL  proc segment/status dump missing ($SEGDUMP / $STATDUMP)"; pass=1
fi

# The "Plan 9 system logs" pipeline grok-parses the syslog line and classifies the
# injected panic into @plan9.event:kernel_panic (source:plan9 logs index on this org).
wait_for "system log classified (@plan9.event:kernel_panic)" 180 event_classified kernel_panic || pass=1

wait_for "httpd CLF logs searchable via pup (best-effort)" 120 logs_searchable ||
	echo "  note  CLF not searchable, best-effort (staging indexing / server pipeline)"

# Diagnostic: show the host's reported OS/platform metadata.
log "host meta (platform should be plan9):"
ddpup infrastructure hosts list --filter="host:${HOST_NAME}" --output json 2>/dev/null |
	jq -c '.data.host_list[0].meta | {platform, agent_version, gohai: (.gohai != null)}' 2>/dev/null || true

if [ "$pass" = 0 ]; then echo "==> PLAN 9 VM E2E PASS"; else echo "==> PLAN 9 VM E2E FAIL"; fi
exit "$pass"
