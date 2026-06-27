# dd-micro-agent

[![CI](https://github.com/0intro/dd-micro-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/0intro/dd-micro-agent/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/0intro/dd-micro-agent.svg)](https://pkg.go.dev/github.com/0intro/dd-micro-agent)

A small, portable agent for metrics and logs, an independent alternative to the
stock Datadog Agent that also runs where the stock Agent does not, most notably
Plan 9. It stands alone, reads the same `datadog.yaml` and `conf.d/*.d/conf.yaml`,
and ships to the same Datadog backend, so existing configs and dashboards keep
working. It does less on purpose, trading features for a small codebase and
portability. Metrics and logs are the core, with host metadata and a few opt-in
extras (Live Processes, a profiling proxy, Plan 9 disk metrics) alongside.

It compiles to one statically-linked binary with just two pure-Go dependencies:
`gopkg.in/yaml.v3` for config and `golang.org/x/sys` for the BSD, macOS, and
Windows system calls (Linux and Plan 9 read kernel state from files and need
neither). No protobuf library, no cgo, no zstd, so `CGO_ENABLED=0 -tags netgo` is
fully static. It is about 11k lines of non-test Go, small enough to read end to
end.

## Disclaimer

This is an independent, unofficial project, not affiliated with or supported by
Datadog, Inc. It talks to the Datadog backend over the public intake APIs, but is
not the official Datadog Agent and carries no warranty. For the supported product,
use the [Datadog Agent](https://github.com/DataDog/datadog-agent).

## What it does

- **DogStatsD** takes metrics over UDP and a Unix socket, aggregates them, and
  flushes every 15s. Counters become rates, histograms and timings expand to
  `.count`/`.avg`/`.median`/`.max`/`.95percentile`, and sets ship a distinct
  count. Only `distribution` is dropped, since it needs DDSketch and protobuf.
  Events (`_e`) and service checks (`_sc`) are forwarded too.
- **Host metrics** cover `system.cpu`, `mem`, `load`, `disk`, `net`, `uptime`, and
  more, read from `/proc` on Linux, `sysctl` and Win32 on the BSDs, macOS, and
  Windows, and kernel text files on Plan 9. Each flush also emits a
  `datadog.agent.running` heartbeat. See [OS support](#os-support).
- **Logs** tails files and, on Linux, the systemd journal (`type: journald`, read
  by exec'ing `journalctl`). File tailing follows rotation and truncation, expands
  globs, picks up new files, and applies per-source `log_processing_rules`
  (`multi_line`, `mask_sequences`, `exclude_at_match`, `include_at_match`). Byte
  offsets and journal cursors persist in `registry.json`, so a restart resumes.
- **Host metadata** sends the v5 `systemStats`/`gohai` payload and the modern
  `inventory_host`/`inventory_agent` payloads, so the host appears in the
  Infrastructure List with a full detail page, OS icon, and tags.
- **Live Processes** *(opt-in)* is the one non-JSON path: a hand-rolled protobuf
  payload, the full list every 10s and realtime stats every 2s on demand, posted
  to `process.<site>`. Enable with `process_config.process_collection.enabled`.
- **Profiling proxy** *(opt-in)* is the only inbound server. It forwards a
  profiler's multipart `/profiling/v1/input` upload unchanged to
  `intake.profile.<site>`, adding the API key and identifying headers. The agent
  profiles nothing itself. Enable with `apm_config.enabled`.
- **venti disk metrics** *(opt-in, Plan 9)* scrape a venti file server's HTTP
  pages into `system.disk.*{device:venti}` and `venti.*` gauges, the only disk
  usage available on Plan 9. Enable with `venti_url`.

Everything is gzipped and authenticated with `DD-API-KEY`, except the process and
profiling paths, which are uncompressed.

| data | endpoint | format |
|------|----------|--------|
| metrics | `POST /api/v1/series` | JSON |
| service checks | `POST /api/v1/check_run` | JSON |
| events and host metadata (v5) | `POST /intake/` | JSON |
| inventory metadata | `POST /api/v1/metadata` | JSON |
| logs | `POST /api/v2/logs` | JSON |
| live processes | `POST process.<site>/api/v1/collector` | protobuf |
| profiling | `POST intake.profile.<site>/api/v2/profile` | multipart passthrough |

## Try it

```sh
make build
DD_API_KEY=<your key> bin/agent --debug
echo "page.views:1|c" | nc -u -w0 127.0.0.1 8125
```

Within a minute the host appears in the Infrastructure List, and
`datadog.agent.running` reads 1, the metric a host-down monitor watches.
`--debug` logs one line per shipment: `metrics flushed`, `log batch sent`,
`process payload sent`.

## What it leaves out

No APM traces, network or security monitoring, DogStatsD distributions or
sketches, autodiscovery, remote config, secrets backends, on-disk retry queue, or
cluster agent. Log sources are files and, on Linux, the systemd journal: no
container, TCP/UDP, or Windows-event channels. If the intake stays unreachable
past the bounded retry (roughly seven seconds of backoff), metric payloads are
dropped and logged, while log batches are held and retried until the intake
accepts them (a payload it refuses outright is dropped and logged). Logs are
at-least-once across restarts: a crash may duplicate a line, never silently lose
one.

## OS support

Builds and runs on Linux, macOS, FreeBSD, OpenBSD, NetBSD, DragonFly BSD, Windows, and Plan 9,
all from one static `CGO_ENABLED=0` binary. Host metadata is collected on all of
them. Host metric coverage is whatever pure Go can read without cgo:

| OS               | CPU | memory | load | disk | network | uptime |
| -----------------|-----|--------|------|------|---------|--------|
| Linux            | ✓   | ✓      | ✓    | ✓    | ✓       | ✓      |
| FreeBSD          | ✓   | ✓      | ✓    | ✓    | ✗       | ✓      |
| macOS            | ✗   | ✗      | ✓    | ✓    | ✗       | ✓      |
| Windows          | ✓   | ✓      | ✗    | ✓    | ✗       | ✓      |
| OpenBSD, NetBSD  | ✓   | ✓      | ✓    | ✓    | ✗       | ✓      |
| DragonFly        | ✓   | ✓      | ✓    | ✓    | ✗       | ✓      |
| Plan 9           | ◐   | ✓      | ◐    | ✗\*  | ◐       | ✓      |

The gaps are the cases that would need cgo. Disk I/O (`system.io.*`, iostat-style
per-device rates) is Linux, FreeBSD, OpenBSD, and NetBSD, plus read and write throughput on DragonFly, swap is Linux and Plan 9, macOS CPU
and memory need Mach, and network throughput is Linux-only, as in the stock Agent.
Plan 9 (◐) reads kernel text files, so some metrics are partial: CPU is idle and
interrupt percent only, load is an instantaneous `system.load.1`, and network is
packet counts, not bytes. It has no kernel disk usage (\*, filled by venti), but
surfaces per-protocol IP/TCP/UDP/ICMP counters, TCP states, process tables, clock
drift, and more. See [Plan 9](#plan-9). Live Processes (opt-in) works on all
eight, each from its own sysctl, kinfo, `/proc`, `ps`, or Win32.

Linux, the BSDs, and Plan 9 are exercised by live VM tests, macOS by a native CI
runner, Windows by cross-compilation and unit tests. Datadog draws an OS icon only
for Linux, macOS, and Windows, so the BSDs and Plan 9 appear fully but without a
glyph, exactly like a stock Agent on those systems.

## Build

```sh
make build          # fully static binary at bin/agent
```

The build needs Go 1.25 or newer and nothing else. Without a checkout,
`go install github.com/0intro/dd-micro-agent/cmd/agent@latest` fetches and
builds it (the binary installs as `agent`).

The `host`, `hostmeta`, and `process` collectors are split per-OS behind build
tags, so keep cross-compilation green when editing them:

```sh
for os in linux darwin windows freebsd openbsd netbsd dragonfly plan9; do GOOS=$os CGO_ENABLED=0 go build ./...; done
```

## Configure

Point it at a `datadog.yaml` (default `/etc/datadog-agent/datadog.yaml`,
`--cfgpath` overrides). Only the keys it understands are read, every other key is
ignored, so a full Agent config works as-is:

```yaml
api_key: <your key>          # required (or DD_API_KEY)
site: datadoghq.com          # intake region
hostname: ""                 # blank resolves like the stock Agent (GCE, OS name, EC2 if default-looking)
tags: [env:prod]             # added to every metric, log, and the host
dogstatsd_port: 8125
logs_enabled: false
enable_metadata_collection: true
```

More are read (`dd_url`, `hostname_file`, `dogstatsd_socket`, `proxy`,
`skip_ssl_validation`, `logs_config` batch tuning), and each has a `DD_*`
environment override (`DD_API_KEY`, `DD_SITE`, `DD_LOGS_ENABLED`, and so on). See
[`internal/config`](internal/config). The agent runs from the environment alone
with no config file, and fails fast if no API key is set.

Three features are opt-in and off by default:

```yaml
venti_url: http://ventihost:8000 # Plan 9: scrape venti disk metrics
process_config:                  # Live Processes
  process_collection: {enabled: true}
apm_config: {enabled: true}      # profiling proxy on :8126
```

Log sources are entries in `conf.d/*.d/conf.yaml`:

```yaml
# /etc/datadog-agent/conf.d/nginx.d/conf.yaml
logs:
  - type: file
    path: /var/log/nginx/access.log   # a glob like /var/log/app/*.log also works
    service: nginx
    source: nginx
  - type: journald                    # Linux only, reads the systemd journal
    include_units: [ssh.service]
```

See [`examples/`](examples) for complete pairs.

### Dual-shipping

The agent can ship the same telemetry to several Datadog orgs at once, via the
stock Agent's `additional_endpoints`. Each destination has its own API key. The
primary stays authoritative: log offsets and flush success track the primary
alone, so a secondary being down never blocks the others. Secondary failures are
logged, best-effort.

```yaml
# metrics, service checks, events, host metadata, and inventory share this map
additional_endpoints:
  "https://app.datadoghq.eu": ["<second org key>"]
```

Logs, Live Processes, and profiling have their own shapes
(`logs_config.additional_endpoints` as a list, and the
`process_config.additional_endpoints` and `apm_config.profiling_additional_endpoints`
maps), each with a JSON-string environment form.

## Run

```sh
bin/agent --cfgpath /etc/datadog-agent/datadog.yaml   # --debug for verbose logging
```

On Linux, [`dist/linux/`](dist/linux) installs the agent as a hardened systemd
service:

```sh
sudo dist/linux/setup.sh bin/agent <your-api-key>   # optional [site], default datadoghq.com
```

`setup.sh` creates a `dd-agent` user, installs the binary and config, keeps the
API key in a 0640 `environment` file out of the world-readable yaml, and enables
the unit. The service runs under systemd sandboxing but still reads `/proc` for
host metrics and Live Processes. Set `LOGS=1` for log collection with a sample
journald source, `PROCESS=1` for Live Processes, and `TAGS='env:prod'` for host
tags.

## Plan 9

Plan 9 is a first-class target. The collectors read kernel text files under
`/dev`, `/net`, and `/proc`, with no syscalls, and emit a wide metric set:
per-protocol network counters, TCP states, process tables, clock drift, CPU
temperature, battery, and more. The kernel keeps no disk-usage or block-I/O
counters, so disk space comes from the opt-in [venti](internal/venti) reporter.
For in-process profiling, where dd-trace-go does not build, a Go program can import
the [`profiler`](profiler) package and upload through the profiling proxy.

[`dist/plan9/`](dist/plan9) has the deployment kit: `setup.rc` (install as a boot
service, write config, send the `platform:plan9` host tag the dashboards filter
on), `dashboard.sh` and `dashboard-networking.sh` (curated dashboards),
`monitors.sh` (starter monitors), and `log-pipeline.sh` (grok pipelines for the
`source:plan9` log lines).

## Tests

```sh
make test     # unit tests, offline
make race     # under the race detector
make vet      # go vet ./...
make fmt      # goimports -w
```

The [`e2e/`](e2e) directory holds live, network-dependent tests, separate from
`go test`. `e2e.sh` runs the real binary against a Datadog org and verifies with
the `pup` CLI. The `vm_*.sh` scripts boot a VM per OS, install the agent, drive
real workloads, and check metrics, logs, metadata, and processes end to end.
`mac.sh` does the same natively on a macOS runner, and `parity.sh` diffs payloads
against a stock Agent. Focused scripts cover journald, profiling, dual-shipping,
and Plan 9 log capture.

## Layout

```
cmd/agent          wiring, signals, graceful shutdown
internal/config    parse datadog.yaml + conf.d, env overrides, endpoint URLs
internal/intake    HTTP transport: gzip, Datadog headers, bounded retry
internal/hostname  config -> hostname_file -> GCE -> os.Hostname -> EC2 (default-looking names only)
internal/metrics   Sample/Serie types, JSON, the aggregator, events, service checks
internal/dogstatsd UDP + Unix-socket listeners, metric/event/service-check parsing
internal/host      host-metric collectors (per-OS: /proc, sysctl, Win32, Plan 9 files)
internal/hostmeta  host metadata: gohai + v5 systemStats and inventory payloads
internal/logs      file + journald tailers, glob discovery, processing rules, registry
internal/process   opt-in Live Processes: hand-rolled protobuf to process.<site>
internal/profiling opt-in profiling proxy: forwards pprof uploads to intake.profile.<site>
internal/venti     opt-in Plan 9 venti disk metrics over HTTP
profiler           public in-process profiler library (dd-trace-go alternative, notably Plan 9)
dist/              Linux and Plan 9 deployment kits
e2e/               live end-to-end, VM, and parity tests
examples/          sample datadog.yaml + conf.d
```

Data flows one way through a few small types. `metrics.Serie` and the log JSON
object are the currency. The aggregator owns the only metrics flush. Host
collectors join it through the `metrics.SeriesSource` interface, so `metrics`
imports nothing from `host`: data flows down, the interface points up. The opt-in
process, profiling, and venti reporters run on their own goroutines, so their
blocking I/O never stalls the flush.

## License

[Apache License 2.0](LICENSE). Copyright 2026 David du Colombier.
