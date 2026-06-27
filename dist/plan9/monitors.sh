#!/usr/bin/env bash
# monitors.sh: create or update a starter set of Datadog monitors for Plan 9 hosts.
#
# These watch the metrics the Plan 9 agent emits. They are intentionally conservative
# and idempotent (matched by name): re-running updates in place. Tune the thresholds
# (and add notification targets, e.g. "@slack-…", to MESSAGE) for your environment.
#
# Env:
#   DD_API_KEY, DD_APP_KEY   required
#   DD_SITE                  Datadog site (default datadoghq.eu)
#   SCOPE                    tag that identifies Plan 9 hosts, default "platform:plan9"
#                            (the host tag setup.rc sends). Set SCOPE= to scope to all hosts.
#   NOTIFY                   text appended to every monitor message, e.g. "@team-plan9".
set -euo pipefail

: "${DD_API_KEY:?set DD_API_KEY}"
: "${DD_APP_KEY:?set DD_APP_KEY}"
SITE="${DD_SITE:-datadoghq.eu}"
SCOPE="${SCOPE-platform:plan9}"
NOTIFY="${NOTIFY:-}"
API="https://api.$SITE/api/v1/monitor"
auth=(-H "DD-API-KEY: $DD_API_KEY" -H "DD-APPLICATION-KEY: $DD_APP_KEY" -H "Content-Type: application/json")

# SC is the metric scope baked into queries: the SCOPE tag, or "*" when disabled.
if [ -n "$SCOPE" ]; then SC="$SCOPE"; else SC="*"; fi

# upsert_monitor creates the monitor described by its JSON arg, or updates the existing
# one with the same name. It validates the JSON and fails the script on an API error.
upsert_monitor() {
	local body="$1" name id out code
	name=$(printf '%s' "$body" | jq -r '.name')
	printf '%s' "$body" | jq -e . >/dev/null || { echo "invalid monitor JSON: $name" >&2; exit 1; }
	id=$(curl -s "${auth[@]}" "$API?name=$(jq -rn --arg n "$name" '$n|@uri')" \
		| jq -r --arg n "$name" '.[]? | select(.name==$n) | .id' | head -n1 || true)
	if [ -n "$id" ] && [ "$id" != null ]; then
		echo "updating monitor $id: $name"
		out=$(curl -s -w '\n%{http_code}' -X PUT "${auth[@]}" -d "$body" "$API/$id")
	else
		echo "creating monitor: $name"
		out=$(curl -s -w '\n%{http_code}' -X POST "${auth[@]}" -d "$body" "$API")
	fi
	code=$(printf '%s' "$out" | tail -n1)
	if [ "$code" != 200 ]; then
		echo "monitor API returned $code for '$name':" >&2
		printf '%s' "$out" | sed '$d' | jq . >&2 2>/dev/null || printf '%s\n' "$out" >&2
		exit 1
	fi
}

tags='["integration:plan9","managed-by:plan9-monitors"]'

# 1) Host/agent down: datadog.agent.running is a constant 1 heartbeat, so its absence
#    means the agent (or host) stopped. notify_no_data is the real trigger here.
upsert_monitor "$(cat <<JSON
{
  "name": "[Plan 9] Agent not reporting",
  "type": "metric alert",
  "query": "max(last_10m):avg:datadog.agent.running{$SC} by {host} < 1",
  "message": "The Plan 9 agent on {{host.name}} has stopped sending its datadog.agent.running heartbeat. The host or agent is likely down. $NOTIFY",
  "tags": $tags,
  "options": {"thresholds": {"critical": 1}, "notify_no_data": true, "no_data_timeframe": 10, "renotify_interval": 0}
}
JSON
)"

# 2) Broken processes: system.proc.count{state:broken} should be 0.
upsert_monitor "$(cat <<JSON
{
  "name": "[Plan 9] Broken processes present",
  "type": "metric alert",
  "query": "max(last_5m):sum:system.proc.count{$SC,state:broken} by {host} >= 1",
  "message": "{{host.name}} has one or more processes in the Broken state (a faulted proc awaiting a debugger). Inspect with 'ps' / acid. $NOTIFY",
  "tags": $tags,
  "options": {"thresholds": {"critical": 1}, "notify_no_data": false, "renotify_interval": 0}
}
JSON
)"

# 3) Venti down: venti.up is 1 when the /storage page answers, 0 when it doesn't.
upsert_monitor "$(cat <<JSON
{
  "name": "[Plan 9] Venti storage down",
  "type": "metric alert",
  "query": "min(last_10m):avg:venti.up{*} by {host} < 1",
  "message": "Venti on {{host.name}} is not answering its /storage page (venti.up=0 or no data). Disk-usage and venti.* metrics will be stale. $NOTIFY",
  "tags": $tags,
  "options": {"thresholds": {"critical": 1}, "notify_no_data": true, "no_data_timeframe": 15, "renotify_interval": 0}
}
JSON
)"

# 4) Venti disk nearly full: system.disk.in_use{device:venti} is a 0-1 ratio.
upsert_monitor "$(cat <<JSON
{
  "name": "[Plan 9] Venti disk usage high",
  "type": "metric alert",
  "query": "avg(last_15m):avg:system.disk.in_use{device:venti} by {host} > 0.9",
  "message": "Venti storage on {{host.name}} is {{value}} full. Venti arenas are append-only. Plan capacity before it fills. $NOTIFY",
  "tags": $tags,
  "options": {"thresholds": {"critical": 0.9, "warning": 0.8}, "notify_no_data": false, "renotify_interval": 0}
}
JSON
)"

# 5) TCP retransmits elevated: tune the threshold to your normal baseline.
upsert_monitor "$(cat <<JSON
{
  "name": "[Plan 9] TCP retransmits elevated",
  "type": "metric alert",
  "query": "avg(last_10m):avg:system.net.tcp.retrans_segs{$SC} by {host} > 50",
  "message": "TCP retransmitted-segment rate on {{host.name}} is {{value}}/s, above the configured threshold. Possible network loss or congestion. Tune this monitor to your baseline. $NOTIFY",
  "tags": $tags,
  "options": {"thresholds": {"critical": 50, "warning": 20}, "notify_no_data": false, "renotify_interval": 0}
}
JSON
)"

# 6) Failed logins from a single source IP, brute-force signal. This is a LOG monitor
#    and depends on the log pipeline (log-pipeline.sh) being applied: it needs the
#    @plan9.event:auth_failure classification and the @network.client.ip facet. Plan 9's
#    auth server logs "tr-fail …" for each rejected ticket request.
upsert_monitor "$(cat <<JSON
{
  "name": "[Plan 9] Failed authentications from a source IP",
  "type": "log alert",
  "query": "logs(\"source:plan9 @plan9.event:auth_failure\").index(\"*\").rollup(\"count\").by(\"@network.client.ip\").last(\"10m\") > 10",
  "message": "A source IP is failing Plan 9 authentication repeatedly ({{value}} in 10m), possible brute force. Check the triggering logs for the IP and user. Requires log-pipeline.sh to be applied. $NOTIFY",
  "tags": $tags,
  "options": {"thresholds": {"critical": 10, "warning": 5}, "enable_logs_sample": true, "notify_no_data": false, "renotify_interval": 0, "groupby_simple_monitor": false}
}
JSON
)"

# 7) Service probes / port scans from a single source IP. Log monitor, depends on the
#    log pipeline (service_scan classification + @network.client.ip facet). Plan 9's
#    ftpd logs "<ip>.<port> <cmd> (<arg>) command not implemented" for scanner probes.
upsert_monitor "$(cat <<JSON
{
  "name": "[Plan 9] Service probes from a source IP",
  "type": "log alert",
  "query": "logs(\"source:plan9 @plan9.event:service_scan\").index(\"*\").rollup(\"count\").by(\"@network.client.ip\").last(\"10m\") > 20",
  "message": "A source IP is probing Plan 9 services with unimplemented FTP/SMTP commands ({{value}} in 10m), port scanner / recon. Requires log-pipeline.sh to be applied. $NOTIFY",
  "tags": $tags,
  "options": {"thresholds": {"critical": 20, "warning": 10}, "enable_logs_sample": true, "notify_no_data": false, "renotify_interval": 0, "groupby_simple_monitor": false}
}
JSON
)"

# 8) Mail relay / spam-source from a single IP (needs log-pipeline.sh applied).
upsert_monitor "$(cat <<JSON
{
  "name": "[Plan 9] Mail relay or spam from a source IP",
  "type": "log alert",
  "query": "logs(\"source:plan9 @plan9.event:(mail_denied OR mail_spam)\").index(\"*\").rollup(\"count\").by(\"@network.client.ip\").last(\"10m\") > 20",
  "message": "A source IP is being denied/blocked repeatedly by the Plan 9 mail server ({{value}} in 10m), relay attempt or spam source. Requires log-pipeline.sh. $NOTIFY",
  "tags": $tags,
  "options": {"thresholds": {"critical": 20, "warning": 10}, "enable_logs_sample": true, "notify_no_data": false, "renotify_interval": 0, "groupby_simple_monitor": false}
}
JSON
)"

# 9) TFTP restricted-path probe from a single IP (boot-server scanning).
upsert_monitor "$(cat <<JSON
{
  "name": "[Plan 9] TFTP restricted-path probe",
  "type": "log alert",
  "query": "logs(\"source:plan9 @plan9.event:tftp_probe\").index(\"*\").rollup(\"count\").by(\"@network.client.ip\").last(\"10m\") > 5",
  "message": "A source IP made {{value}} bad/restricted TFTP requests in 10m, boot-server probing. $NOTIFY",
  "tags": $tags,
  "options": {"thresholds": {"critical": 5, "warning": 2}, "enable_logs_sample": true, "notify_no_data": false, "renotify_interval": 0, "groupby_simple_monitor": false}
}
JSON
)"

# 10) cron ownership mismatch: the privilege-escalation guard tripping is always notable.
upsert_monitor "$(cat <<JSON
{
  "name": "[Plan 9] cron ownership mismatch",
  "type": "log alert",
  "query": "logs(\"source:plan9 @plan9.event:cron_privesc\").index(\"*\").rollup(\"count\").last(\"15m\") >= 1",
  "message": "A Plan 9 cron job tripped the ownership / dangerous-host guard (cron for X owned by Y), possible privilege-escalation attempt. $NOTIFY",
  "tags": $tags,
  "options": {"thresholds": {"critical": 1}, "enable_logs_sample": true, "notify_no_data": false, "renotify_interval": 0}
}
JSON
)"

# 11) secstore access denied / failed operations from a source IP.
upsert_monitor "$(cat <<JSON
{
  "name": "[Plan 9] secstore access denied or failed",
  "type": "log alert",
  "query": "logs(\"source:plan9 @plan9.event:secstore_denied\").index(\"*\").rollup(\"count\").by(\"@network.client.ip\").last(\"10m\") > 3",
  "message": "Repeated denied/failed secstore operations ({{value}} in 10m), possible secret-store probing or tampering. $NOTIFY",
  "tags": $tags,
  "options": {"thresholds": {"critical": 3, "warning": 1}, "enable_logs_sample": true, "notify_no_data": false, "renotify_interval": 0, "groupby_simple_monitor": false}
}
JSON
)"

# 12) Malicious attachment blocked by the mail virus filter.
upsert_monitor "$(cat <<JSON
{
  "name": "[Plan 9] Mail attachment blocked",
  "type": "log alert",
  "query": "logs(\"source:plan9 @plan9.event:attachment_blocked\").index(\"*\").rollup(\"count\").last(\"15m\") >= 1",
  "message": "The Plan 9 mail virus filter rejected an executable/dangerous attachment. Review the sender and recipient. $NOTIFY",
  "tags": $tags,
  "options": {"thresholds": {"critical": 1}, "enable_logs_sample": true, "notify_no_data": false, "renotify_interval": 0}
}
JSON
)"

echo "done."
