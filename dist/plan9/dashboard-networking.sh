#!/usr/bin/env bash
# dashboard-networking.sh: create or replace "Plan 9 - Networking", a strict Plan 9
# equivalent of Datadog's stock "System - Networking" dashboard.
#
# That dashboard is seven timeseries of system.net.bytes_{rcvd,sent} sliced four ways
# (aggregate, avg/min/max, by host, by device). Plan 9 counts **packets, not bytes**
# (the kernel's /net/etherN/stats has no byte counters), so this mirrors the exact
# widget structure with system.net.packets_{in,out}.count, the same shape, the unit
# Plan 9 actually exposes. Like the stock one it uses a $scope template variable
# (defaulted here to Plan 9 hosts). Ordered layout with auto reflow, no per-widget
# coordinates, identical to the source.
#
# Env:
#   DD_API_KEY, DD_APP_KEY   required
#   DD_SITE                  Datadog site (default datadoghq.eu)
#   SCOPE                    default value of the $scope template variable. Default
#                            "platform:plan9" (set by the agent's host metadata).
#                            Set SCOPE=* to show every host, as the stock dashboard does.
set -euo pipefail

: "${DD_API_KEY:?set DD_API_KEY}"
: "${DD_APP_KEY:?set DD_APP_KEY}"
SITE="${DD_SITE:-datadoghq.eu}"
SCOPE="${SCOPE-platform:plan9}"
TITLE="Plan 9 - Networking"
API="https://api.$SITE/api/v1/dashboard"

template=$(cat <<'JSON'
{
  "title": "@@TITLE@@",
  "description": "Strict Plan 9 equivalent of the stock System - Networking dashboard. Plan 9 counts packets, not bytes, so it tracks system.net.packets_{in,out}.count. Use the $scope template variable to filter.",
  "layout_type": "ordered",
  "reflow_type": "auto",
  "template_variables": [
    {"name": "scope", "available_values": [], "default": "@@SCOPE@@"}
  ],
  "widgets": [
    {"definition": {"title": "Network traffic (packets/sec)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "sum:system.net.packets_in.count{$scope}", "display_type": "line"},
        {"q": "sum:system.net.packets_out.count{$scope}", "display_type": "line"}]}},

    {"definition": {"title": "Packets in (per sec, avg/min/max)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.packets_in.count{$scope}", "display_type": "line"},
        {"q": "min:system.net.packets_in.count{$scope}", "display_type": "line"},
        {"q": "max:system.net.packets_in.count{$scope}", "display_type": "line"}]}},

    {"definition": {"title": "Packets out (per sec, avg/min/max)", "type": "timeseries", "show_legend": true,
      "requests": [
        {"q": "avg:system.net.packets_out.count{$scope}", "display_type": "line"},
        {"q": "min:system.net.packets_out.count{$scope}", "display_type": "line"},
        {"q": "max:system.net.packets_out.count{$scope}", "display_type": "line"}]}},

    {"definition": {"title": "Packets in (per second, by host)", "type": "timeseries", "show_legend": true,
      "requests": [{"q": "system.net.packets_in.count{$scope} by {host}", "display_type": "line"}]}},

    {"definition": {"title": "Packets out (per second, by host)", "type": "timeseries", "show_legend": true,
      "requests": [{"q": "system.net.packets_out.count{$scope} by {host}", "display_type": "line"}]}},

    {"definition": {"title": "Packets in (per second, by device)", "type": "timeseries", "show_legend": true,
      "requests": [{"q": "system.net.packets_in.count{$scope} by {device}", "display_type": "line"}]}},

    {"definition": {"title": "Packets out (per second, by device)", "type": "timeseries", "show_legend": true,
      "requests": [{"q": "system.net.packets_out.count{$scope} by {device}", "display_type": "line"}]}}
  ]
}
JSON
)

dash=$(printf '%s' "$template" | sed -e "s|@@TITLE@@|$TITLE|g" -e "s|@@SCOPE@@|$SCOPE|g")

# Fail early on a malformed body rather than shipping it.
printf '%s' "$dash" | jq -e . >/dev/null || { echo "generated dashboard JSON is invalid" >&2; exit 1; }

auth=(-H "DD-API-KEY: $DD_API_KEY" -H "DD-APPLICATION-KEY: $DD_APP_KEY" -H "Content-Type: application/json")

# Replace the existing dashboard (keeping its URL) if one exists, else create.
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
