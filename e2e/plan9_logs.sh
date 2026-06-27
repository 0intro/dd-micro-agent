#!/usr/bin/env bash
#
# Capture REAL Plan 9 service logs for the "Plan 9 system logs" pipeline
# (dist/plan9/log-pipeline.sh). It boots the ID=9 9legacy clone
# (../../contrib/9legacy-9) under QEMU with a virtio-9p root + ELF kernel via that
# tree's boot/qemu -386, driven by expect exactly like e2e/vm_plan9.sh, then stands up
# the services whose /sys/log/* lines are not yet grok-parsed and drives a loopback
# client at each so they emit genuine lines:
#
#   auth      auth/changeuser installs a user        "user X installed for plan 9"
#   pop3      a good login + 3 bad logins            "user X logged in" / "likely password guesser from IP"
#   smtp.fail an outbound send to a bogus domain     "delivery to X at Y failed: ..."
#   runq      the queue runner over that message     "no data file for ..." / queue activity
#   dnsq      a UDP query to a Logqueries=1 dns       "id N: (IP/port) id qname qtype"
#   ssh       a loopback ssh login (aux/sshserve)    "connect from" / "logged in as X"
#   pptpd     ip/pptpd started (best effort)         startup line only (no GRE peer under SLIRP)
#   6in4      ip/6in4 started (best effort)          startup/error line only (no tunnel peer)
#
# There is NO Datadog round trip and NO API key: the virtio-9p root IS the clone, so the
# guest writes /sys/log/* straight onto the host and we read them back from
# $CKOUT/sys/log/* after the run. Those captured lines are the ground truth for the grok
# rules and become the in-pipeline test samples.
#
# "ID=9" is the contrib concurrent-instance key: the clone
# is 9legacy-9. SLIRP user-net is private per VM, so boot/qemu's fixed MAC is harmless.
# We keep our own scratch + serial paths so this runs alongside the e2e/build VMs.
#
# dnsq is compiled OFF in stock 9legacy (Logqueries=0, no runtime flag), so we patch the
# clone's sys/src/cmd/ndb/dnudpserver.c to Logqueries=1 and rebuild dns in the guest. The
# patch is local to the clone. Revert it with: git -C ../../contrib/9legacy-9 checkout -- sys/src/cmd/ndb/dnudpserver.c
#
#   PLAN9_CLONE   the ID=9 clone (default ../../contrib/9legacy-9)
#   KEEP          1 = leave the dnudpserver.c patch in place (default 0 = revert)
#
# Needs KVM, qemu-system-x86_64, and expect. Run with the sandbox OFF (the guest dials
# 10.0.2.3 for the bogus-domain MX lookup that makes smtp.fail fire).
#
set -uo pipefail

# Clone github.com/0intro/9legacy (override PLAN9_REPO with a local working copy), cached
# under PLAN9_CACHE and bootstrapped with boot/getbin if absent, so this runs anywhere.
REPO="${PLAN9_REPO:-https://github.com/0intro/9legacy}"
CACHE="${PLAN9_CACHE:-/tmp/plan9-9legacy}"
CKOUT="${PLAN9_CLONE:-$CACHE/9legacy-9}"
W=/tmp/microagent-plan9-logs
DD=/usr/glenda/dd                 # guest path (on the virtio-9p root) for our files
GUESTDD="$CKOUT$DD"               # the same directory as seen on the host
SERIAL="$W/serial.log"
SYSNAME=9legacy-386               # boot/qemu sets sysname=9legacy-$arch
AUTHDOM=9legacy
PASS=plan9logs                    # keyfs master key == glenda's password (throwaway VM)
KEEP="${KEEP:-0}"
DNUDP="$CKOUT/sys/src/cmd/ndb/dnudpserver.c"

# Every /sys/log file a service may write. /sys/log is sys-owned, so glenda can't create
# there. Pre-create them host-side (the 9p root is host-writable) so the guest's syslog(2)
# appends land and we can read them back.
LOGS=(pop3 smtp smtp.fail runq auth ssh dnsq pptpd 6in4 smtpd mail cs)

log() { echo "==> $*"; }
cleanup() {
	pkill -f "path=$CKOUT,security_model" 2>/dev/null
	[ "$KEEP" = 1 ] || git -C "$CKOUT" checkout -- sys/src/cmd/ndb/dnudpserver.c 2>/dev/null
}
trap cleanup EXIT

for c in qemu-system-x86_64 expect git 9660srv 9p; do
	command -v "$c" >/dev/null || { echo "$c missing (9660srv/9p come from plan9port)"; exit 1; }
done
if [ ! -f "$CKOUT/386/9pcf.elf" ]; then
	log "cloning + installing 9legacy into $CKOUT (first run downloads the CD)"
	mkdir -p "$CACHE"
	[ -d "$CKOUT/.git" ] || git clone --depth 1 "$REPO" "$CKOUT" || { echo "clone failed"; exit 1; }
	( cd "$CKOUT" && ./boot/mkdirs && ./boot/getbin ) || { echo "boot/getbin failed"; exit 1; }
	[ -f "$CKOUT/386/9pcf.elf" ] || { echo "386/9pcf.elf missing after getbin"; exit 1; }
fi
[ -w /dev/kvm ] || echo "warning: /dev/kvm not writable; QEMU will be slow"
pkill -f "path=$CKOUT,security_model" 2>/dev/null && sleep 1
rm -rf "$W"; mkdir -p "$W" "$GUESTDD" "$CKOUT/usr/glenda/bin/rc"
log "clone=$CKOUT sysname=$SYSNAME"

# Logqueries is a compile-time constant. Flip it to 1 so dnudpserver logs each query.
if grep -q 'Logqueries = 0' "$DNUDP"; then
	log "patching dnudpserver.c Logqueries 0 -> 1 (reverted on exit unless KEEP=1)"
	sed -i 's/Logqueries = 0/Logqueries = 1/' "$DNUDP"
fi

for f in "${LOGS[@]}"; do : >"$CKOUT/sys/log/$f"; chmod 0666 "$CKOUT/sys/log/$f"; done
# /mail and /sys/log are sys-owned, so glenda can't mkdir there in-guest. Pre-create the
# upas queue/box dirs host-side (the 9p root is host-writable) so the guest can write them.
mkdir -p "$CKOUT/mail/queue/glenda" "$CKOUT/mail/box/glenda"
chmod -R 0777 "$CKOUT/mail/queue" "$CKOUT/mail/box" 2>/dev/null || true

# Non-interactive: networking, the auth-domain ndb, rebuild dns with the query-log patch,
# minimal upas config, and a no-TLS pop3 service. Notes go to /dev/cons so we can follow
# progress on the serial console.
cat > "$CKOUT/usr/glenda/bin/rc/ddprep" <<EOF
#!/bin/rc
rfork e
DD=$DD
fn note { echo '### '\$1 >/dev/cons }

note net
bind -a '#I' /net >[2]/dev/null
bind -a '#l0' /net >[2]/dev/null
ip/ipconfig -g 10.0.2.2 ether /net/ether0 10.0.2.15 255.255.255.0 >[2]/dev/null
ip/ipconfig loopback /dev/null 127.0.0.1 >[2]/dev/null
ndb/cs >[2]/dev/null

note ndb
echo 'authdom=$AUTHDOM auth=127.0.0.1' >>/lib/ndb/local
echo 'dns=10.0.2.3' >>/lib/ndb/local
echo 'hostid=glenda' >>/lib/ndb/auth
echo '	uid=!sys uid=!adm uid=*' >>/lib/ndb/auth

note dnsbuild
@{ cd /sys/src/cmd/ndb && mk 8.dns && cp 8.dns /386/bin/ndb/dns } >\$DD/dnsbuild.log >[2=1]
# termrc already started an unpatched ndb/dns -r. Kill it and post our query-logging one
# as the resolver + UDP server (only -s starts dnudpserver, which logs each query).
note dnsstart
@{ kill dns | rc } >[2]/dev/null
rm -f /srv/dns
sleep 1
ndb/dns -rs >\$DD/dns.log >[2=1] &
sleep 2

note mailcfg
mkdir -p /mail/queue/glenda /mail/box/glenda
touch /mail/box/glenda/mbox
chmod 666 /mail/box/glenda/mbox
cp /mail/lib/rewrite.direct /mail/lib/rewrite >[2]/dev/null
echo 'glenda	glenda' >>/mail/lib/names.local >[2]/dev/null
# pop3 -p permits cleartext USER/PASS (no TLS in this loopback test)
echo '#!/bin/rc' >/rc/bin/service/tcp110
echo 'exec upas/pop3 -p' >>/rc/bin/service/tcp110
chmod +x /rc/bin/service/tcp110
note prepdone
EOF
chmod 0755 "$CKOUT/usr/glenda/bin/rc/ddprep"

# After auth is up (keyfs + changeuser + factotum key, driven interactively below): start
# the listeners, then drive a loopback client at each service. Best effort: nothing is
# allowed to fail the run. Every stage tees stderr to $DD and we dump /sys/log/* at the end.
cat > "$CKOUT/usr/glenda/bin/rc/ddrun" <<EOF
#!/bin/rc
rfork e
DD=$DD
fn note { echo '### '\$1 >/dev/cons }

note authsrv
mv /rc/bin/service.auth/authsrv.tcp567 /rc/bin/service.auth/tcp567 >[2]/dev/null
aux/listen -t /rc/bin/service.auth -d /rc/bin/service tcp >\$DD/listen.log >[2=1] &
sleep 3
# host rsa+dsa keys so netssh can complete the handshake (else "no proto=... key"), plus
# glenda's ssh password key so the loopback client authenticates (ssh2 uses proto=pass
# service=ssh) and the server logs the login.
auth/rsagen >/mnt/factotum/ctl >[2]\$DD/rsagen.log
auth/dsagen >/mnt/factotum/ctl >[2]\$DD/dsagen.log
echo 'key proto=pass service=ssh user=glenda !password=$PASS' >/mnt/factotum/ctl >[2]/dev/null
sleep 1

# con on a tcp! dest does rlogin auth first, which mangles a line protocol. -l runs the
# raw "simple" path first (it eats the dest as remuser, then recovers it as dest), and -C
# (cooked) skips the consctl raw-mode dance. So -C -l gives a clean byte stream.
note pop3good
@{ echo USER glenda; sleep 1; echo PASS $PASS; sleep 1; echo STAT; sleep 1; echo QUIT; sleep 1 } | con -C -l tcp!127.0.0.1!110 >>\$DD/pop3client.log >[2=1]
sleep 1
note pop3bad
@{ echo USER glenda; sleep 1; echo PASS wrong1; sleep 1; echo USER glenda; sleep 1; echo PASS wrong2; sleep 1; echo USER glenda; sleep 1; echo PASS wrong3; sleep 1; echo QUIT; sleep 1 } | con -C -l tcp!127.0.0.1!110 >>\$DD/pop3client.log >[2=1]
sleep 1

# smtp.fail: dial a closed local port so the failure is a deterministic connection
# refusal, with no DNS (the rebuilt dns -rs crashes its resolver slaves on this kernel
# when it goes upstream. The dnsq stage below tolerates that since it logs on receive).
note smtpfail
echo 'test body' | upas/smtp -h $SYSNAME tcp!127.0.0.1!2525 glenda@$AUTHDOM user@example.com >>\$DD/smtpclient.log >[2=1]
sleep 1
# a server that answers then 5xx-rejects, for the richer "delivery to X at Y failed: reply" shape
aux/listen1 -t 'tcp!*!2526' rc -c '{echo 554 5.7.1 rejected; sleep 3}' >[2]/dev/null &
sleep 1
echo 'test body' | upas/smtp -h $SYSNAME tcp!127.0.0.1!2526 glenda@$AUTHDOM user@bad.example.com >>\$DD/smtpclient.log >[2=1]
sleep 1

# runq descends into a per-user subdir (/mail/queue/<user>/), scanning C.* entries. A
# ctl file with no matching D. data file yields the deterministic "no data file" line.
note runq
echo 'queued ctl with no data' >/mail/queue/glenda/C.ddtest123
upas/runq /mail/queue /mail/lib/remotemail >>\$DD/runqclient.log >[2=1]
sleep 1

# dnsq: the UDP server announces on the ether IP (10.0.2.15), not loopback, so query
# that. It logs each query (Logqueries) on receive, before resolution, so we get the line
# even if the resolver then fails. Background it so a hung dnsdebug can't stall the run.
note dnsq
ndb/dnsdebug @10.0.2.15 example.com ip >>\$DD/dnsqclient.log >[2=1] &
sleep 2
ndb/dnsdebug @10.0.2.15 microagent.test ip >>\$DD/dnsqclient.log >[2=1] &
sleep 2

# ssh: aux/sshserve (tcp22) logs "connect from IP" the moment a connection is accepted,
# before auth, so a raw connect alone yields a real line, then a full ssh client login
# (glenda's key is in factotum) for the "logged in as" line. Both backgrounded with
# /dev/null stdin so neither can block.
note ssh
echo quit | con -C -l tcp!127.0.0.1!22 >>\$DD/sshconnect.log >[2=1] &
sleep 2
ssh 127.0.0.1 'echo hello-from-ssh' </dev/null >>\$DD/sshclient.log >[2=1] &
sleep 4

note pptp6in4
ip/pptpd >\$DD/pptpd.log >[2=1] &
ip/6in4 2001:db8:1::1/64 192.0.2.1 2001:db8:2::1 >\$DD/6in4.log >[2=1] &
sleep 2

note dumplogs
for(f in /sys/log/*){ echo '=====LOG '\$f >/dev/cons; cat \$f >/dev/cons; echo >/dev/cons }
echo DDLOGS-DONE >/dev/cons
EOF
chmod 0755 "$CKOUT/usr/glenda/bin/rc/ddrun"

# Same driver as vm_plan9.sh: expect spawns boot/qemu -386, waits for the rc prompt, then
# runs ddprep, the interactive auth dance (keyfs -p prompts for the master key, changeuser
# walks the howto dialogue and writes /sys/log/auth), and ddrun. PASS/AUTHDOM are baked in.
log "booting the ID=9 clone and capturing service logs (several minutes)"
: >"$SERIAL"
PASS="$PASS" AUTHDOM="$AUTHDOM" SERIAL="$SERIAL" \
	expect -f - "$CKOUT/boot/qemu" -386 <<'EXPECT'
log_file -a $env(SERIAL)
set timeout 300
set pass $env(PASS)
set authdom $env(AUTHDOM)
spawn -noecho {*}$argv
set send_slow {1 0.02}
proc bail {code} { catch {exec kill [exp_pid]}; exit $code }
proc die {msg} { puts stderr "\nplan9_logs: $msg"; bail 1 }
proc at {} {
	expect {
		-exact "term% " {}
		timeout { die "no shell prompt" }
		eof { die "qemu exited" }
	}
}
proc cmd {s} { send -s -- $s; send "\r"; at }

# wait for the boot to settle on the first console prompt
at
sleep 1

# prep: net, ndb, rebuild dns (slow), upas config
set timeout 360
cmd "rc /usr/glenda/bin/rc/ddprep"

# auth: keyfs prompts once for the master key (getpass on /dev/cons), then daemonizes
set timeout 120
send -s -- "auth/keyfs -p -m /mnt/keys /adm/keys"; send "\r"
expect {
	-re {[Pp]assword[^:]*: ?} { send -s -- "$pass\r" }
	timeout { die "keyfs: no password prompt" }
	eof { die "qemu exited" }
}
at

# changeuser: the howto dialogue (writes /sys/log/auth "user glenda installed for plan 9")
send -s -- "auth/changeuser glenda"; send "\r"
expect -re {Password[^:]*: ?};	send -s -- "$pass\r"
expect -re {Confirm[^:]*: ?};	send -s -- "$pass\r"
expect -re {Inferno/POP secret\? \(y/n\) ?};	send -s -- "y\r"
expect -re {same as the plan 9 password\? \(y/n\) ?};	send -s -- "y\r"
expect -re {Expiration date[^:]*: ?};	send -s -- "\r"
expect -re {Post id[^:]*: ?};	send -s -- "\r"
expect -re {full name[^:]*: ?};	send -s -- "\r"
expect -re {Department[^:]*: ?};	send -s -- "\r"
expect -re {email address[^:]*: ?};	send -s -- "\r"
expect -re {Sponsor[^:]*: ?};	send -s -- "\r"
at

# factotum holds glenda's key so the loopback ssh client can authenticate
cmd "echo 'key proto=p9sk1 dom=$authdom user=glenda !password=$pass' >/mnt/factotum/ctl"

# run: listeners + one loopback client per service, then dump /sys/log/* to the console.
# Generous timeout: pop3's brute-force backoff sleeps (5s, 10s, 20s) make pop3bad slow.
set timeout 420
send -s -- "rc /usr/glenda/bin/rc/ddrun"; send "\r"
expect {
	"DDLOGS-DONE" {}
	timeout { die "ddrun timed out" }
	eof { die "qemu exited" }
}
at
bail 0
EXPECT
rc=$?
[ "$rc" -ne 0 ] && { echo "  guest run failed (rc=$rc); serial tail:"; tail -50 "$SERIAL"; }

echo
log "captured /sys/log on the host (the ground truth for the grok rules):"
for f in "${LOGS[@]}"; do
	p="$CKOUT/sys/log/$f"
	if [ -s "$p" ]; then echo "----- /sys/log/$f"; cat "$p"; fi
done
echo
log "per-stage client logs (debug):"
for s in dnsbuild pop3client smtpclient runqclient dnsqclient sshclient listen pptpd 6in4; do
	p="$GUESTDD/$s.log"
	[ -s "$p" ] && { echo "----- $s.log"; sed -n '1,20p' "$p"; }
done
log "done (serial log at $SERIAL)"
