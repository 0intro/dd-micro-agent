#!/usr/bin/env bash
# log-pipeline.sh. Create or replace the two Plan 9 Datadog log pipelines:
#
#   1. "Plan 9 httpd (CLF)"  (service:httpd). Grok-parses ip/httpd's Common Log
#      Format and its native "SYSNAME Mon DD HH:MM:SS IP.PORT method uri status"
#      syslog access line into @http.*/@network.client.*, then geo-IP-resolves the
#      client IP into @network.client.geoip.* for the geomap widget.
#      All Plan 9 /sys/log files (auth, smtpd, mail, ipboot, secstore, cron, ftp, ...)
#      arrive here under one tag set, so disambiguation is by line content.
#   2. "Plan 9 system logs"  (source:plan9). Grok-parses @plan9.sysname/date/msg plus
#      the structured service lines: auth (tr/cr/chap/securid/RADIUS, account install),
#      mail (smtpd delivery/verdict/auth/greeting, local delivery, vf, outbound
#      smtp.fail, runq queue), pop3 (login, brute-force), ssh (sshserve/netssh connection
#      + login), network-boot (dhcp lease, tftp transfer/probe), secstore session, ftp,
#      listener, cron/cpu exec, ppp, dnsq query log, pptpd VPN, 6in4 tunnel, timesync
#      clock-drift, cs.paranoia name-lookup audit, telnet/rlogind, trampoline port-forward,
#      9down/sources, imap4d, fax, telco, extracting @network.client.ip (geo-IP-mapped) +
#      @mail.*/@auth.*/@ssh.*/@dns.*/@vpn.*/@telnet.*/@http.*/@timesync.*/@dhcp.*/@exec.*/
#      @secstore.* so events group and map by source. A category processor classifies
#      @plan9.event (kernel_panic, auth_ok/auth_failure, account_created,
#      pop3_login/pop3_bruteforce, ssh_login/ssh_failure, mail_*, mail_deliver_fail,
#      mail_queue, dns_query/dns_error, dhcp_nak, tftp_probe, secstore_denied,
#      cron_privesc, service_scan, ftp_login, ppp_authfail, vpn_call, tunnel_filtered,
#      clock_drift, name_lookup_audit, telnet_login, port_forward_denied, web_request,
#      imap_error, fax_event, modem_event, cs_error)
#      and a status remapper raises panics to error and the failures/scans/spam to
#      warning. /sys/log/* and kmesg ship source:plan9.
#
#   The grok samples below double as regression cases. Most are real lines captured by
#   booting Plan 9 under QEMU and driving each server (e2e/plan9_logs.sh). The
#   dnsq/pptpd/6in4 rules are derived from the daemons' source format strings: a single
#   SLIRP VM can't reach dnudpserver (it only logs external clients) or exercise PPTP/6in4
#   (no GRE / tunnel peer), so those lines were verified in source, not live-captured.
#
# Pipelines apply at ingestion, so they affect logs received after creation, not
# already-indexed ones.
#
# Env: DD_API_KEY, DD_APP_KEY (required). DD_SITE (default datadoghq.eu).
#      HTTPD_FILTER (default service:httpd). SYS_FILTER (default source:plan9).
set -euo pipefail

: "${DD_API_KEY:?set DD_API_KEY}"
: "${DD_APP_KEY:?set DD_APP_KEY}"
SITE="${DD_SITE:-datadoghq.eu}"
API="https://api.$SITE/api/v1/logs/config/pipelines"
auth=(-H "DD-API-KEY: $DD_API_KEY" -H "DD-APPLICATION-KEY: $DD_APP_KEY" -H "Content-Type: application/json")

# pipeline 1: httpd CLF + native access line
HTTPD_NAME="Plan 9 httpd (CLF)"
HTTPD_FILTER="${HTTPD_FILTER:-service:httpd}"
# Datadog grok for Common Log Format plus the native plan9 access line (single-quoted
# so backslashes/quotes stay literal, jq escapes it for JSON below).
HTTPD_GROK='clf %{ipOrHost:network.client.ip} %{notSpace:http.ident} %{notSpace:http.auth} \[%{date("dd/MMM/yyyy:HH:mm:ss Z"):date_access}\] "%{word:http.method} %{notSpace:http.url} HTTP/%{number:http.version}" %{integer:http.status_code} %{integer:network.bytes_written}
plan9access %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{ipv4:network.client.ip}.%{integer:network.client.port} %{data:plan9.action}
plan9overload %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} overloaded by %{ipv4:network.client.ip}
plan9httpd %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{data:plan9.msg}'

httpd_body=$(jq -n --arg name "$HTTPD_NAME" --arg filter "$HTTPD_FILTER" --arg grok "$HTTPD_GROK" '{
  name: $name, is_enabled: true, filter: {query: $filter},
  processors: [
    {type: "grok-parser", name: "Parse CLF access lines", is_enabled: true, source: "message",
     samples: ["127.0.0.1 - - [10/Oct/2024:13:55:36 +0000] \"GET /apache_pb.gif HTTP/1.0\" 200 2326",
               "cetus Dec  5 16:41:35 217.17.237.197.39849 get /cm/cs/what/feaver/webgui.html OK"],
     grok: {support_rules: "", match_rules: $grok}},
    {type: "geo-ip-parser", name: "Geo-IP the client IP", is_enabled: true,
     sources: ["network.client.ip"], target: "network.client.geoip"}
  ]
}')

# pipeline 2: Plan 9 system logs (syslog + event classification)
SYS_NAME="Plan 9 system logs"
SYS_FILTER="${SYS_FILTER:-source:plan9}"
# Match rules are tried in order. First match wins, so specific shapes precede the
# generic syslog catch-all. Rules that capture a client IP feed the geo-IP processor
# (so every source maps). Groups: auth (tr/cr/chap/securid/RADIUS), mail (smtpd
# delivery/verdict/auth/greeting, local delivery, vf), network-boot (dhcp, tftp),
# secstore session, ftp, listener, cron/cpu exec, ppp. Unmatched lines (incl. failures
# with no IP, like "tr-fail authid not found") fall through to plan9syslog (@plan9.msg).
# NB: no literal "'" anywhere. It would close the single-quoted bash string / jq program.
SYS_GROK='plan9auth %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{word:auth.proto}-%{word:auth.result} %{data:auth.user}@%{data:auth.dom}\(%{ipv4:network.client.ip}\)%{data:plan9.msg}
plan9authhostid %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{word:auth.proto}-%{word:auth.result} hostid %{data:auth.user}\(%{ipv4:network.client.ip}\)%{data:plan9.msg}
plan9authat %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{word:auth.proto}-%{word:auth.result} %{data:auth.user}@%{ipv4:network.client.ip}%{data:plan9.msg}
plan9authsp %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{word:auth.proto}-%{word:auth.result} %{notSpace:auth.user} %{ipv4:network.client.ip}%{data:plan9.msg}
plan9radius %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{data:plan9.radserver} %{word:auth.result} ruser=%{notSpace:auth.user}%{data:plan9.msg}
plan9smtpdeliv %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} \[%{data:mail.helo}/%{ipv4:network.client.ip}\] %{data:mail.sender} sent %{integer:mail.bytes} bytes to %{data:mail.rcpt}
plan9smtpverdict %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{word:mail.action} %{data:mail.sender} \(%{data:mail.helo}/%{ipv4:network.client.ip}\) \(%{data:mail.rcpt}\)
plan9smtpaction %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{word:mail.action} %{data:mail.sender} \(%{data:mail.helo}/%{ipv4:network.client.ip}\)%{data:plan9.msg}
plan9smtpauth %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} auth\(%{data:mail.authmech}, %{data:auth.user}\) from %{ipv4:network.client.ip}%{data:plan9.msg}
plan9smtpgreet %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{notSpace:mail.cmd} from %{ipv4:network.client.ip} as %{data:mail.helo}
plan9maildeliv %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} delivered %{notSpace:mail.rcpt} From %{data:mail.sender} %{data:mail.deliverdate} %{integer:mail.bytes}
plan9vf %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} vf rejected %{notSpace:mail.mimetype} %{data:mail.attachment}
plan9smtpclient %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{data:mail.sender} sent %{integer:mail.bytes} bytes to %{data:mail.rcpt}
plan9dhcp %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{word:dhcp.type}\(%{data:dhcp.range}\)id\(%{data:dhcp.clientid}\)ci\(%{data:dhcp.ciaddr}\)gi\(%{data:dhcp.giaddr}\)yi\(%{ipv4:dhcp.assigned_ip}\)si\(%{data:dhcp.siaddr}\)%{data:plan9.msg}
plan9tftpbad %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} tftpd %{integer:tftp.pid} bad request %{integer:tftp.opcode} from %{ipv4:network.client.ip}%{data:tftp.client} file %{data:tftp.file}
plan9tftp %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} tftpd %{integer:tftp.pid} %{notSpace:tftp.action} file %{notSpace:tftp.file} %{notSpace:tftp.mode} to %{ipv4:network.client.ip}%{data:plan9.msg}
plan9secauth %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} AUTH %{notSpace:secstore.user}%{data:plan9.msg}
plan9secop %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} \[%{notSpace:secstore.user}\] %{notSpace:secstore.op}%{data:secstore.target}
plan9ftpget %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{ipv4:network.client.ip}\.%{integer:network.client.port} get %{data:ftp.file} OK %{integer:ftp.bytes}
plan9ftp %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{ipv4:network.client.ip}\.%{integer:network.client.port} %{notSpace:ftp.command} %{data:plan9.msg}
plan9secstore %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} secstore from %{ipv4:network.client.ip}!%{integer:network.client.port}%{data:plan9.msg}
plan9connect %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} connected from %{ipv4:network.client.ip}%{data:plan9.msg}
plan9croncall %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{data:exec.user}: called %{data:exec.cmd} on %{notSpace:exec.host}%{data:plan9.msg}
plan9cronauth %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{data:exec.user}: authenticated %{data:exec.cmd} on %{notSpace:exec.host}%{data:plan9.msg}
plan9ppp %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} : remote=%{ipv4:network.client.ip}: auth %{notSpace:ppp.result}: uid=%{notSpace:auth.user}%{data:plan9.msg}
plan9cronown %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} cron for %{notSpace:cron.user} owned by %{data:cron.owner}
plan9exec %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{data:exec.user}: ran %{data:exec.cmd}
plan9pop3login %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} user %{notSpace:auth.user} logged in
plan9authinstall %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} user %{notSpace:auth.user} installed for %{data:auth.realm}
plan9pop3guess %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} likely password guesser from %{ipv4:network.client.ip}
plan9smtpfailto %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{word:smtp.phase} to %{notSpace:mail.dest} failed: %{data:mail.error}
plan9smtpfailnet %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{data:mail.error} \(%{regex("[a-z]+![^)]+"):mail.dest}\)
plan9runqnodata %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} no data file for %{notSpace:mail.queue_id}
plan9runqrm %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} removing %{notSpace:mail.queue_path}
plan9sshnetuser %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} netssh: client user %{notSpace:ssh.user}@%{ipv4:network.client.ip} id %{integer:ssh.connid} %{data:ssh.msg}
plan9sshsession %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} server %{data:ssh.action} for %{notSpace:ssh.user}
plan9sshloggedin %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} logged in as %{notSpace:ssh.user}
plan9sshconnfrom %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} connect from %{ipv4:network.client.ip}%{data:plan9.msg}
plan9sshnet %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} netssh: %{data:ssh.msg}
plan9dnsq %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} id %{integer:dns.id}: \(%{ipv4:network.client.ip}/%{integer:network.client.port}\) %{integer:dns.msgid} %{notSpace:dns.query_name} %{notSpace:dns.query_type}
plan9pptpcall %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} : src=%{ipv4:vpn.client_ip}: call started: id=%{integer:vpn.call_id}: remote ip=%{ipv4:vpn.remote_ip}
plan9pptpclose %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} : src=%{ipv4:vpn.client_ip}: call closed: id=%{integer:vpn.call_id}: send=%{integer:vpn.send} sendack=%{integer:vpn.sendack} recv=%{integer:vpn.recv} recvack=%{integer:vpn.recvack} dropped=%{integer:vpn.dropped} missing=%{integer:vpn.missing} sendwait=%{integer:vpn.sendwait} sendtimeout=%{integer:vpn.sendtimeout}
plan96in4 %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{word:tunnel.direction} filtered %{notSpace:tunnel.src6} -> %{notSpace:tunnel.dst6}; %{data:tunnel.reason}
plan9timesync %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} δ %{integer:timesync.offset} avgδ %{integer:timesync.offset_avg} hz %{integer:timesync.hz}
plan9csparanoia %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} write %{data:dns.lookup} by %{notSpace:auth.user}
plan9telnet %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} \(%{ipv4:network.client.ip}\) %{data:telnet.line}
plan9rlogind %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} rlogind %{notSpace:auth.user}
plan9trampoline %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} badmac %{notSpace:trampoline.mac} from %{ipv4:network.client.ip}!%{integer:network.client.port} for %{data:trampoline.target}
plan9downreject %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} reject %{ipv4:network.client.ip} %{notSpace:http.url}
plan9down %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{word:plan9.service} %{ipv4:network.client.ip} ver %{number:http.version} uri %{notSpace:http.url} search %{data:plan9.msg}
plan9imap4d %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} imap4d at %{integer:imap.at}: %{data:plan9.msg}
plan9fax %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{regex("success|failed"):fax.result} %{regex("[0-9+][^ ]*"):fax.number} %{data:fax.file}
plan9telco %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} dialing %{notSpace:telco.number} speed=%{integer:telco.speed}%{data:plan9.msg}
plan9syslog %{notSpace:plan9.sysname} %{regex("[A-Za-z]+ +[0-9]+ +[0-9:]+"):plan9.date} %{data:plan9.msg}'

sys_body=$(jq -n --arg name "$SYS_NAME" --arg filter "$SYS_FILTER" --arg grok "$SYS_GROK" '{
  name: $name, is_enabled: true, filter: {query: $filter},
  processors: [
    {type: "grok-parser", name: "Parse the Plan 9 syslog / service lines", is_enabled: true, source: "message",
     samples: ["auriga Jun 21 23:14:33 tr-ok web@web(83.202.21.228) -> web@bootes",
               "cetus Dec  5 16:41:35 panic: fault: 0 consecutive faults for va 0x0",
               "9legacy-386 Jun 22 16:45:51 user glenda logged in",
               "9legacy-386 Jun 22 16:45:42 user glenda installed for plan 9",
               "9legacy-386 Jun 22 16:08:05 likely password guesser from 127.0.0.1",
               "9legacy-386 Jun 22 16:46:05 delivery to tcp!127.0.0.1!2526 failed: 554 5.7.1 rejected",
               "9legacy-386 Jun 23 10:00:00 δ -1234 avgδ 5678 hz 1000000000",
               "9legacy-386 Jun 22 16:46:09 no data file for C.ddtest123",
               "9legacy-386 Jun 22 16:46:16 netssh: client user glenda@127.0.0.1 id 0 connect handshake failed: tcp conn 23",
               "cetus Jun 22 10:00:00 id 5: (203.0.113.7/53) 12345 example.com A"],
     grok: {support_rules: "", match_rules: $grok}},
    {type: "geo-ip-parser", name: "Geo-IP the client IP", is_enabled: true,
     sources: ["network.client.ip"], target: "network.client.geoip"},
    {type: "category-processor", name: "Classify notable events", is_enabled: true,
     target: "plan9.event",
     categories: [
       {name: "kernel_panic", filter: {query: "panic OR kfault OR \"consecutive faults\""}},
       {name: "auth_failure", filter: {query: "\"tr-fail\" OR \"cr-fail\" OR \"apop-fail\" OR \"chap-fail\" OR \"mschap-fail\" OR \"vnc-fail\" OR \"cram-fail\" OR \"g-fail\" OR \"cp-fail\" OR \"rejected ruser\""}},
       {name: "auth_ok",      filter: {query: "\"tr-ok\" OR \"cr-ok\" OR \"apop-ok\" OR \"chap-ok\" OR \"mschap-ok\" OR \"cram-ok\" OR \"g-ok\" OR \"vnc-ok\" OR \"accepted ruser\""}},
       {name: "auth_malformed", filter: {query: "\"unknown ticket request\""}},
       {name: "secstore_denied", filter: {query: "\"secstore denied\" OR \"no STA\" OR \"failed PUT\" OR \"failed RM\" OR \"illegal name\" OR \"protocol botch\""}},
       {name: "attachment_blocked", filter: {query: "\"vf rejected\" OR \"validateattachment rejected\""}},
       {name: "tftp_probe",   filter: {query: "\"bad request\""}},
       {name: "dhcp_nak",     filter: {query: "\"!Request\" OR \"!Discover\" OR \"not valid for\""}},
       {name: "cron_privesc", filter: {query: "\"owned by\" OR \"dangerous host\""}},
       {name: "service_scan", filter: {query: "\"command not implemented\""}},
       {name: "ftp_login",    filter: {query: "\"user anonymous\" OR \"login none\" OR \"login anonymous\""}},
       {name: "dns_error",    filter: {query: "servfail OR \"dns failure\" OR \"bad delegation\""}},
       {name: "ppp_authfail", filter: {query: "\"auth failed\""}},
       {name: "ssh_failure",  filter: {query: "\"handshake failed\" OR \"ID too short\" OR \"no sshserve keys\" OR \"no proto\""}},
       {name: "ssh_login",    filter: {query: "\"logged in as\" OR \"ssh shell for\" OR \"ssh session for\""}},
       {name: "pop3_bruteforce", filter: {query: "\"password guesser\""}},
       {name: "pop3_login",   filter: {query: "\"logged in\""}},
       {name: "account_created", filter: {query: "\"installed for plan\" OR \"installed for securenet\""}},
       {name: "mail_deliver_fail", filter: {query: "(failed AND (delivery OR ping)) OR \"connection refused\" OR \"closed pipe\""}},
       {name: "mail_queue",   filter: {query: "\"no data file\" OR \"out of procs\" OR \"empty ctl\" OR \"returnmail child\""}},
       {name: "dns_query",    filter: {query: "@dns.query_name:*"}},
       {name: "vpn_call",     filter: {query: "\"call started\" OR \"call closed\" OR \"pptp started\""}},
       {name: "tunnel_filtered", filter: {query: "\"egress filtered\" OR \"ingress filtered\""}},
       {name: "clock_drift",  filter: {query: "@timesync.hz:* OR \"no sample\""}},
       {name: "name_lookup_audit", filter: {query: "@dns.lookup:*"}},
       {name: "telnet_login", filter: {query: "@telnet.line:* OR rlogind"}},
       {name: "port_forward_denied", filter: {query: "badmac"}},
       {name: "web_request",  filter: {query: "@http.url:*"}},
       {name: "imap_error",   filter: {query: "@imap.at:*"}},
       {name: "fax_event",    filter: {query: "@fax.result:*"}},
       {name: "modem_event",  filter: {query: "@telco.number:*"}},
       {name: "cs_error",     filter: {query: "\"format error\" OR \"unknown request\""}},
       {name: "cron_error",   filter: {query: "cron AND (error OR failed OR \"no access\")"}},
       {name: "mail_spam",    filter: {query: "spammer OR Dumped OR \"names.blocked\""}},
       {name: "mail_bounce",  filter: {query: "\"error+\" OR returnmail"}},
       {name: "mail_denied",  filter: {query: "Denied OR Refused OR Rejected OR Blocked OR Disallowed OR \"Bad Forward\""}},
       {name: "mail_delivered", filter: {query: "\"bytes to\" OR delivered"}}
     ]},
    {type: "category-processor", name: "Derive a severity", is_enabled: true,
     target: "plan9.severity",
     categories: [
       {name: "error",   filter: {query: "@plan9.event:kernel_panic"}},
       {name: "warning", filter: {query: "@plan9.event:(auth_failure OR mail_denied OR mail_spam OR mail_bounce OR dns_error OR cron_error OR cron_privesc OR service_scan OR tftp_probe OR secstore_denied OR attachment_blocked OR ppp_authfail OR dhcp_nak OR pop3_bruteforce OR account_created OR mail_deliver_fail OR ssh_failure OR tunnel_filtered OR port_forward_denied)"}}
     ]},
    {type: "status-remapper", name: "Status from severity", is_enabled: true, sources: ["plan9.severity"]}
  ]
}')

# create or replace each pipeline by name
upsert_pipeline() {
  local name="$1" body="$2" id out code resp
  id=$(curl -s "${auth[@]}" "$API" | jq -r --arg n "$name" '.[]? | select(.name==$n) | .id' | head -n1 || true)
  if [ -n "$id" ]; then
    echo "replacing pipeline '$name' ($id)"
    out=$(curl -s -w '\n%{http_code}' -X PUT "${auth[@]}" -d "$body" "$API/$id")
  else
    echo "creating pipeline '$name'"
    out=$(curl -s -w '\n%{http_code}' -X POST "${auth[@]}" -d "$body" "$API")
  fi
  code=$(printf '%s' "$out" | tail -n1)
  resp=$(printf '%s' "$out" | sed '$d')
  if [ "$code" != 200 ]; then
    echo "pipeline API returned $code for '$name':" >&2
    printf '%s\n' "$resp" | jq . >&2 2>/dev/null || printf '%s\n' "$resp" >&2
    return 1
  fi
  echo "ok: pipeline '$name' id=$(printf '%s' "$resp" | jq -r .id)"
}

upsert_pipeline "$HTTPD_NAME" "$httpd_body"
upsert_pipeline "$SYS_NAME" "$sys_body"
