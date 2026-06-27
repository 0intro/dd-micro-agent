#!/usr/bin/env bash
# dashboard.sh: create or replace the "Plan 9" Datadog dashboard.
#
# Shows the Plan 9 host metrics, lists the Plan 9 hosts, streams their logs, and
# carries a $host template variable so the whole page filters per host.
#
# Env:
#   DD_API_KEY, DD_APP_KEY   required
#   DD_SITE                  Datadog site (default datadoghq.eu)
#   SCOPE                    metric tag that identifies Plan 9 hosts, default
#                            "platform:plan9" (a real host tag the agent sends, setup.rc adds it.
#                            Datadog does NOT derive a metric tag from metadata platform).
#                            Override if your hosts carry a different tag, e.g.
#                            SCOPE=os:plan9. Set SCOPE= to disable the filter.
#   LOGSCOPE                 log query that selects Plan 9 logs, default
#                            "source:plan9". Set LOGSCOPE= to disable.
set -euo pipefail

: "${DD_API_KEY:?set DD_API_KEY}"
: "${DD_APP_KEY:?set DD_APP_KEY}"
SITE="${DD_SITE:-datadoghq.eu}"
SCOPE="${SCOPE-platform:plan9}"
LOGSCOPE="${LOGSCOPE-source:plan9}"
TITLE="Plan 9"
API="https://api.$SITE/api/v1/dashboard"

# $host is a Datadog template variable (kept literal). SCOPE/LOGSCOPE are baked in.
if [ -n "$SCOPE" ]; then MF="$SCOPE,\$host"; else MF="\$host"; fi
if [ -n "$LOGSCOPE" ]; then LQ="$LOGSCOPE \$host"; else LQ="\$host"; fi

template=$(cat <<'JSON'
{
  "title": "@@TITLE@@",
  "description": "Plan 9 hosts: metrics and logs. Use the $host variable to filter per host.",
  "layout_type": "ordered",
  "template_variables": [
    {"name": "host", "prefix": "host", "available_values": [], "default": "*"}
  ],
  "widgets": [
    {"definition": {"title": "Plan 9 hosts", "type": "toplist",
      "requests": [{"q": "avg:system.uptime{@@MF@@} by {host}"}]},
     "layout": {"x": 0, "y": 0, "width": 4, "height": 4}},

    {"definition": {"title": "CPU idle %", "type": "timeseries", "show_legend": true,
      "requests": [{"q": "avg:system.cpu.idle{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 4, "y": 0, "width": 8, "height": 4}},

    {"definition": {"title": "Load (1m and per-core)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.load.1{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.load.norm.1{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 0, "y": 4, "width": 6, "height": 4}},

    {"definition": {"title": "Memory (MB)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.mem.total{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.mem.used{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.mem.free{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 6, "y": 4, "width": 6, "height": 4}},

    {"definition": {"title": "Memory usable (ratio 0-1)", "type": "timeseries", "show_legend": true,
      "requests": [{"q": "avg:system.mem.pct_usable{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 0, "y": 8, "width": 6, "height": 4}},

    {"definition": {"title": "Swap (MB)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.swap.total{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.swap.used{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.swap.free{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 6, "y": 8, "width": 6, "height": 4}},

    {"definition": {"title": "Network (packets/s, by host and device)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.packets_in.count{@@MF@@} by {host,device}", "display_type": "line"},
        {"q": "avg:system.net.packets_out.count{@@MF@@} by {host,device}", "display_type": "line"}]},
     "layout": {"x": 0, "y": 12, "width": 8, "height": 4}},

    {"definition": {"title": "CPU cores", "type": "timeseries", "show_legend": true,
      "requests": [{"q": "avg:system.cpu.num_cores{@@MF@@} by {host}", "display_type": "bars"}]},
     "layout": {"x": 8, "y": 12, "width": 4, "height": 4}},

    {"definition": {"title": "CPU activity (per second)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.cpu.context_switches{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.cpu.interrupts{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.cpu.syscalls{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.cpu.faults{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 0, "y": 16, "width": 6, "height": 4}},

    {"definition": {"title": "Network errors (per second, by device)", "type": "timeseries", "show_legend": true,
      "requests": [{"q": "avg:system.net.errors.count{@@MF@@} by {host,device}", "display_type": "line"}]},
     "layout": {"x": 6, "y": 16, "width": 6, "height": 4}},

    {"definition": {"title": "TCP established & half-open (limbo)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.tcp.current_established{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.tcp.in_limbo{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 0, "y": 20, "width": 6, "height": 4}},

    {"definition": {"title": "TCP retransmits & resets (per second)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.tcp.retrans_segs{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.tcp.established_resets{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 6, "y": 20, "width": 6, "height": 4}},

    {"definition": {"title": "Top processes by memory (MB)", "type": "toplist",
      "requests": [{"q": "avg:system.proc.memory{@@MF@@} by {proc}"}]},
     "layout": {"x": 0, "y": 24, "width": 6, "height": 4}},

    {"definition": {"title": "Venti disk (kB: total / used / free)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.disk.total{device:venti,$host} by {host}", "display_type": "line"},
        {"q": "avg:system.disk.used{device:venti,$host} by {host}", "display_type": "line"},
        {"q": "avg:system.disk.free{device:venti,$host} by {host}", "display_type": "line"}]},
     "layout": {"x": 6, "y": 24, "width": 6, "height": 4}},

    {"definition": {"title": "Plan 9 events by type (auth, panics, …)", "type": "timeseries", "show_legend": true,
      "requests": [{
        "response_format": "timeseries",
        "queries": [{
          "name": "events", "data_source": "logs",
          "search": {"query": "@@LQ@@ @plan9.event:*"},
          "indexes": ["*"],
          "group_by": [{"facet": "@plan9.event", "limit": 10, "sort": {"aggregation": "count", "order": "desc"}}],
          "compute": {"aggregation": "count"}}],
        "formulas": [{"formula": "events"}],
        "display_type": "bars"}]},
     "layout": {"x": 0, "y": 28, "width": 12, "height": 4}},

    {"definition": {"title": "Auth attempts by source IP (top)", "type": "toplist",
      "requests": [{
        "response_format": "scalar",
        "queries": [{
          "name": "auth", "data_source": "logs",
          "search": {"query": "@@LQ@@ @plan9.event:(auth_ok OR auth_failure)"},
          "indexes": ["*"],
          "group_by": [{"facet": "@network.client.ip", "limit": 15, "sort": {"aggregation": "count", "order": "desc"}}],
          "compute": {"aggregation": "count"}}],
        "formulas": [{"formula": "auth"}]}]},
     "layout": {"x": 0, "y": 30, "width": 6, "height": 4}},

    {"definition": {"title": "Inbound sources by country (auth/ftp/secstore)", "type": "geomap",
      "requests": [{
        "response_format": "scalar",
        "queries": [{
          "name": "src", "data_source": "logs",
          "search": {"query": "@@LQ@@ @network.client.ip:*"},
          "indexes": ["*"],
          "group_by": [{"facet": "@network.client.geoip.country.iso_code", "limit": 250, "sort": {"aggregation": "count", "order": "desc"}}],
          "compute": {"aggregation": "count"}}],
        "formulas": [{"formula": "src"}]}],
      "style": {"palette": "hostmap_blues", "palette_flip": false},
      "view": {"focus": "WORLD"}},
     "layout": {"x": 6, "y": 30, "width": 6, "height": 4}},

    {"definition": {"title": "HTTP requests (httpd access logs)", "type": "timeseries", "show_legend": true,
      "requests": [{
        "response_format": "timeseries",
        "queries": [{
          "name": "requests", "data_source": "logs",
          "search": {"query": "service:httpd (GET OR POST OR HEAD OR PUT OR DELETE OR OPTIONS OR PATCH) $host"},
          "indexes": ["*"],
          "group_by": [{"facet": "host", "limit": 20, "sort": {"aggregation": "count", "order": "desc"}}],
          "compute": {"aggregation": "count"}}],
        "formulas": [{"formula": "requests"}],
        "display_type": "bars"}]},
     "layout": {"x": 0, "y": 32, "width": 12, "height": 4}},

    {"definition": {"title": "HTTP access logs", "type": "log_stream",
      "query": "service:httpd $host",
      "columns": ["host", "@network.client.ip", "@http.method", "@http.status_code"],
      "show_date_column": true, "show_message_column": true,
      "message_display": "inline",
      "sort": {"column": "time", "order": "desc"}},
     "layout": {"x": 0, "y": 36, "width": 12, "height": 6}},

    {"definition": {"title": "HTTP clients by country", "type": "geomap",
      "requests": [{
        "response_format": "scalar",
        "queries": [{
          "name": "clients", "data_source": "logs",
          "search": {"query": "service:httpd $host"},
          "indexes": ["*"],
          "group_by": [{"facet": "@network.client.geoip.country.iso_code", "limit": 250, "sort": {"aggregation": "count", "order": "desc"}}],
          "compute": {"aggregation": "count"}}],
        "formulas": [{"formula": "clients"}]}],
      "style": {"palette": "hostmap_blues", "palette_flip": false},
      "view": {"focus": "WORLD"}},
     "layout": {"x": 0, "y": 42, "width": 12, "height": 5}},

    {"definition": {"title": "Plan 9 logs", "type": "log_stream",
      "query": "@@LQ@@",
      "columns": ["host", "service", "@plan9.event"],
      "show_date_column": true, "show_message_column": true,
      "message_display": "inline",
      "sort": {"column": "time", "order": "desc"}},
     "layout": {"x": 0, "y": 47, "width": 12, "height": 6}},

    {"definition": {"title": "Venti cache hit rate (index & disk)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"response_format": "timeseries", "display_type": "line",
         "queries": [
           {"name": "ih", "data_source": "metrics", "query": "sum:venti.icache.hits{$host}"},
           {"name": "im", "data_source": "metrics", "query": "sum:venti.icache.misses{$host}"}],
         "formulas": [{"formula": "ih / (ih + im)", "alias": "index cache"}]},
        {"response_format": "timeseries", "display_type": "line",
         "queries": [
           {"name": "dh", "data_source": "metrics", "query": "sum:venti.dcache.hits{$host}"},
           {"name": "dm", "data_source": "metrics", "query": "sum:venti.dcache.misses{$host}"}],
         "formulas": [{"formula": "dh / (dh + dm)", "alias": "disk cache"}]}]},
     "layout": {"x": 0, "y": 53, "width": 8, "height": 4}},

    {"definition": {"title": "Venti compression ratio", "type": "query_value", "precision": 2,
      "requests": [{"q": "avg:venti.data.compression_ratio{$host}", "aggregator": "last"}]},
     "layout": {"x": 8, "y": 53, "width": 4, "height": 4}},

    {"definition": {"title": "Venti RPC (bytes/s & errors/s)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:venti.rpc.read_bytes{$host} by {host}", "display_type": "line"},
        {"q": "avg:venti.rpc.write_bytes{$host} by {host}", "display_type": "line"},
        {"q": "avg:venti.rpc.read_fails{$host} by {host}", "display_type": "bars"},
        {"q": "avg:venti.rpc.write_fails{$host} by {host}", "display_type": "bars"}]},
     "layout": {"x": 0, "y": 57, "width": 12, "height": 4}},

    {"definition": {"title": "TCP connections by state", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.tcp.current_established{@@MF@@} by {host}", "display_type": "area"},
        {"q": "avg:system.net.tcp.listen{@@MF@@} by {host}", "display_type": "area"},
        {"q": "avg:system.net.tcp.time_wait{@@MF@@} by {host}", "display_type": "area"},
        {"q": "avg:system.net.tcp.close_wait{@@MF@@} by {host}", "display_type": "area"},
        {"q": "avg:system.net.tcp.syn_sent{@@MF@@} by {host}", "display_type": "area"},
        {"q": "avg:system.net.tcp.closing{@@MF@@} by {host}", "display_type": "area"}]},
     "layout": {"x": 0, "y": 61, "width": 12, "height": 4}},

    {"definition": {"title": "IP throughput (datagrams/s)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.ip.in_receives{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.ip.in_delivers{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.ip.out_requests{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.ip.forwarded_datagrams{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 0, "y": 65, "width": 6, "height": 4}},

    {"definition": {"title": "IP errors & reassembly (per second)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.ip.in_discards{@@MF@@} by {host}", "display_type": "bars"},
        {"q": "avg:system.net.ip.in_header_errors{@@MF@@} by {host}", "display_type": "bars"},
        {"q": "avg:system.net.ip.reassembly_fails{@@MF@@} by {host}", "display_type": "bars"},
        {"q": "avg:system.net.ip.fragmentation_fails{@@MF@@} by {host}", "display_type": "bars"}]},
     "layout": {"x": 6, "y": 65, "width": 6, "height": 4}},

    {"definition": {"title": "ICMP messages (per second)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.icmp.in_msgs{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.icmp.out_msgs{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.icmp.in_errors{@@MF@@} by {host}", "display_type": "bars"}]},
     "layout": {"x": 0, "y": 69, "width": 4, "height": 4}},

    {"definition": {"title": "UDP datagrams (per second)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.udp.in_datagrams{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.udp.out_datagrams{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.udp.in_errors{@@MF@@} by {host}", "display_type": "bars"},
        {"q": "avg:system.net.udp.no_ports{@@MF@@} by {host}", "display_type": "bars"}]},
     "layout": {"x": 4, "y": 69, "width": 4, "height": 4}},

    {"definition": {"title": "Routing & ARP table sizes", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.iproute.count{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.arp.entries{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 8, "y": 69, "width": 4, "height": 4}},

    {"definition": {"title": "Interface IP packets (per second, by device)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.iface.packets_in{@@MF@@} by {host,device}", "display_type": "line"},
        {"q": "avg:system.net.iface.packets_out{@@MF@@} by {host,device}", "display_type": "line"}]},
     "layout": {"x": 0, "y": 73, "width": 6, "height": 4}},

    {"definition": {"title": "CPU idle % per core", "type": "heatmap",
      "requests": [{"q": "avg:system.cpu.idle{@@MF@@} by {cpu}"}]},
     "layout": {"x": 6, "y": 73, "width": 6, "height": 4}},

    {"definition": {"title": "Processes by state", "type": "timeseries", "show_legend": true,
      "requests": [{"q": "avg:system.proc.count{@@MF@@} by {state}", "display_type": "area"}]},
     "layout": {"x": 0, "y": 77, "width": 12, "height": 4}},

    {"definition": {"title": "TCP opens & segments (per second)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.tcp.active_opens{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.tcp.passive_opens{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.tcp.in_segs{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.net.tcp.out_segs{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 0, "y": 81, "width": 6, "height": 4}},

    {"definition": {"title": "Swap free (ratio 0-1)", "type": "timeseries", "show_legend": true,
      "requests": [{"q": "avg:system.swap.pct_free{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 6, "y": 81, "width": 6, "height": 4}},

    {"definition": {"title": "CPU temperature (°C, per core)", "type": "timeseries", "show_legend": true,
      "requests": [{"q": "avg:system.cpu.temp{@@MF@@} by {host,cpu}", "display_type": "line"}]},
     "layout": {"x": 0, "y": 85, "width": 6, "height": 4}},

    {"definition": {"title": "Kernel memory pools (MB)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.mem.kernel.malloc{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.mem.kernel.malloc.max{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.mem.kernel.draw{@@MF@@} by {host}", "display_type": "line"},
        {"q": "avg:system.mem.kernel.draw.max{@@MF@@} by {host}", "display_type": "line"}]},
     "layout": {"x": 6, "y": 85, "width": 6, "height": 4}},

    {"definition": {"title": "Security & service events by type", "type": "timeseries", "show_legend": true,
      "requests": [{
        "response_format": "timeseries",
        "queries": [{
          "name": "ev", "data_source": "logs",
          "search": {"query": "@@LQ@@ @plan9.event:(auth_failure OR mail_denied OR mail_spam OR mail_bounce OR tftp_probe OR secstore_denied OR cron_privesc OR attachment_blocked OR service_scan OR ppp_authfail OR dhcp_nak OR dns_error)"},
          "indexes": ["*"],
          "group_by": [{"facet": "@plan9.event", "limit": 15, "sort": {"aggregation": "count", "order": "desc"}}],
          "compute": {"aggregation": "count"}}],
        "formulas": [{"formula": "ev"}],
        "display_type": "bars"}]},
     "layout": {"x": 0, "y": 89, "width": 12, "height": 4}}
  ]
}
JSON
)

dash=$(printf '%s' "$template" \
  | sed -e "s|@@TITLE@@|$TITLE|g" -e "s|@@MF@@|$MF|g" -e "s|@@LQ@@|$LQ|g")

# Fail early on a malformed body rather than shipping it.
printf '%s' "$dash" | jq -e . >/dev/null || { echo "generated dashboard JSON is invalid" >&2; exit 1; }

auth=(-H "DD-API-KEY: $DD_API_KEY" -H "DD-APPLICATION-KEY: $DD_APP_KEY" -H "Content-Type: application/json")

# Replace the existing "Plan 9" dashboard (keeping its URL) if one exists, else create.
id=$(curl -s "${auth[@]}" "$API" | jq -r --arg t "$TITLE" '.dashboards[]? | select(.title==$t) | .id' | head -n1 || true)
if [ -n "$id" ]; then
	echo "replacing existing dashboard $id"
	out=$(curl -s -w '\n%{http_code}' -X PUT "${auth[@]}" -d "$dash" "$API/$id")
else
	echo "creating new dashboard"
	out=$(curl -s -w '\n%{http_code}' -X POST "${auth[@]}" -d "$dash" "$API")
fi

code=$(printf '%s' "$out" | tail -n1)
body=$(printf '%s' "$out" | sed '$d')
if [ "$code" != 200 ]; then
	echo "dashboard API returned $code:" >&2
	printf '%s\n' "$body" | jq . >&2 2>/dev/null || printf '%s\n' "$body" >&2
	exit 1
fi
echo "ok: https://app.$SITE$(printf '%s' "$body" | jq -r '.url // empty')"
