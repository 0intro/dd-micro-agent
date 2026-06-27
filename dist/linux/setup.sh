#!/bin/sh
# setup.sh: install dd-micro-agent as a systemd service on a Linux host.
#
# Usage:  sudo ./setup.sh <agentbin> <apikey> [site]
#
#   agentbin  the built linux binary (make build => bin/agent)
#   apikey    Datadog API key, written to /etc/datadog-agent/environment (0640)
#   site      Datadog site (default datadoghq.com)
#
# Env knobs:
#   TAGS='env:prod role:web'  extra host tags, space separated
#   LOGS=1     set logs_enabled and drop a sample journald source in conf.d
#   PROCESS=1  enable Live Processes and install the CAP_SYS_PTRACE drop-in
#
# Creates the dd-agent system user, installs the binary to
# /opt/datadog-agent/bin/agent, writes /etc/datadog-agent/{datadog.yaml,environment},
# and installs and enables datadog-agent.service. The API key is kept in the
# environment file (mode 0640) so it never lands in the world-readable yaml. Run
# as root.
set -eu

TAGS="${TAGS:-}"
LOGS="${LOGS:-}"
PROCESS="${PROCESS:-}"

usage() {
	echo 'usage: sudo ./setup.sh <agentbin> <apikey> [site]' >&2
	exit 2
}

[ $# -eq 2 ] || [ $# -eq 3 ] || usage
agentbin=$1
apikey=$2
site=${3:-datadoghq.com}

if [ "$(id -u)" -ne 0 ]; then
	echo 'setup.sh: must run as root' >&2
	exit 1
fi
if [ ! -f "$agentbin" ]; then
	echo "setup.sh: no such file: $agentbin" >&2
	exit 1
fi

# The unit and drop-in are shipped beside this script.
scriptdir=$(cd "$(dirname "$0")" && pwd)

# 1. system user, idempotent, with the same flags the stock Agent uses.
getent group dd-agent >/dev/null || groupadd --system dd-agent
getent passwd dd-agent >/dev/null || useradd --system --gid dd-agent \
	--home-dir /opt/datadog-agent --no-create-home --shell /usr/sbin/nologin dd-agent

# 2. layout. run_path is the only directory the agent writes (registry.json).
install -d -m 0755 /opt/datadog-agent/bin
install -d -m 0750 -o dd-agent -g dd-agent /opt/datadog-agent/run
install -d -m 0755 /etc/datadog-agent/conf.d

# 3. binary.
install -m 0755 "$agentbin" /opt/datadog-agent/bin/agent

# 4. datadog.yaml, root owned and world readable (it holds no secret, the key is
#    in the environment file).
logs_enabled=false
[ "$LOGS" = 1 ] && logs_enabled=true
{
	echo '# api_key is read from DD_API_KEY in /etc/datadog-agent/environment.'
	echo "site: $site"
	if [ -n "$TAGS" ]; then
		echo 'tags:'
		for t in $TAGS; do
			echo "  - $t"
		done
	fi
	echo 'dogstatsd_port: 8125'
	echo 'dogstatsd_non_local_traffic: false'
	echo "logs_enabled: $logs_enabled"
	echo 'confd_path: /etc/datadog-agent/conf.d'
	echo 'run_path: /opt/datadog-agent/run'
	echo 'enable_metadata_collection: true'
	if [ "$PROCESS" = 1 ]; then
		echo 'process_config:'
		echo '  process_collection:'
		echo '    enabled: true'
	fi
} >/etc/datadog-agent/datadog.yaml
chmod 0644 /etc/datadog-agent/datadog.yaml

# 5. API key in the environment file, readable only by root and dd-agent. The
#    umask keeps it from ever being world readable, even for the instant before
#    the chmod.
(umask 077; printf 'DD_API_KEY=%s\n' "$apikey" >/etc/datadog-agent/environment)
chown root:dd-agent /etc/datadog-agent/environment
chmod 0640 /etc/datadog-agent/environment

# 6. unit.
install -m 0644 "$scriptdir/datadog-agent.service" /etc/systemd/system/datadog-agent.service

# 7. Live Processes drop-in (CAP_SYS_PTRACE), present only when PROCESS=1.
if [ "$PROCESS" = 1 ]; then
	install -d -m 0755 /etc/systemd/system/datadog-agent.service.d
	install -m 0644 "$scriptdir/liveprocesses.conf" \
		/etc/systemd/system/datadog-agent.service.d/liveprocesses.conf
	echo 'PROCESS=1, enabling Live Processes with the CAP_SYS_PTRACE drop-in'
else
	rm -f /etc/systemd/system/datadog-agent.service.d/liveprocesses.conf
fi

# 8. sample log source when LOGS=1. Left alone if it already exists, so a re-run
#    does not clobber edits.
if [ "$LOGS" = 1 ] && [ ! -e /etc/datadog-agent/conf.d/journald.yaml ]; then
	cat >/etc/datadog-agent/conf.d/journald.yaml <<'EOF'
# Sample journald log source. Add file sources beside it as needed, for example
# {type: file, path: /var/log/nginx/access.log, service: nginx, source: nginx}.
logs:
  - type: journald
    source: systemd
    service: systemd
EOF
	echo 'LOGS=1, wrote a sample journald source to conf.d/journald.yaml'
fi

# 9. start it. restart, not just enable, so a re-run with a new key or tags takes effect.
systemctl daemon-reload
systemctl enable datadog-agent
systemctl restart datadog-agent

echo
echo 'setup complete. inspect it with:'
echo '	systemctl status datadog-agent'
echo '	journalctl -u datadog-agent -f'
