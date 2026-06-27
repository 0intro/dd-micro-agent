// Package venti reports Plan 9 venti storage and server metrics as Datadog metrics.
// Venti is the only Plan 9 file server that exposes usage without a 9P console: it
// serves plaintext status pages over HTTP. We scrape its /storage page (disk space,
// arenas, clumps, compression) on every host, and (since venti's /stats endpoint is
// commented out upstream) a selected set of performance counters via its /graph
// interface (which only yields data when venti runs with collectstats, the default).
//
// This reporter is opt-in (the agent runs it only when venti_url is set) and runs on
// its own goroutine so its HTTP I/O never stalls the metrics flush. It is plain
// net/http + text parsing, so it builds on every OS and unit-tests on the dev host.
package venti

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// Options configures a Reporter.
type Options struct {
	Client           *intake.Client
	MetricsEndpoints []intake.Endpoint // the /api/v1/series endpoints (primary + additional)
	VentiURL         string            // venti http base, e.g. http://127.0.0.1:8000
	Hostname         string
	Tags             []string
	Interval         time.Duration // defaults to 15s
	Logger           *slog.Logger
}

// Reporter periodically scrapes venti and ships its metrics.
type Reporter struct {
	client    *intake.Client
	endpoints []intake.Endpoint // /api/v1/series (primary + additional)
	base      string            // venti http base (no trailing slash)
	hostname  string
	tags      []string
	interval  time.Duration
	http      *http.Client
	log       *slog.Logger

	// per-second rate state for the cumulative /graph counters
	prev   map[string]uint64
	prevTs time.Time
}

// New returns a Reporter.
func New(o Options) *Reporter {
	if o.Interval == 0 {
		o.Interval = 15 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Reporter{
		client:    o.Client,
		endpoints: o.MetricsEndpoints,
		base:      strings.TrimRight(o.VentiURL, "/"),
		hostname:  o.Hostname,
		tags:      o.Tags,
		interval:  o.Interval,
		http:      &http.Client{Timeout: 3 * time.Second}, // never wedge on a slow venti
		log:       o.Logger,
	}
}

// Run scrapes once at startup, then on the interval until ctx is cancelled.
func (r *Reporter) Run(ctx context.Context) {
	r.send(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.send(ctx)
		}
	}
}

func (r *Reporter) send(ctx context.Context) {
	now := time.Now()
	st, err := r.fetchStorage(ctx)
	if err != nil {
		r.log.Warn("venti storage fetch failed", "url", r.base+"/storage", "err", err)
		// Ship venti.up=0 so a down venti shows as a clear signal rather than a silent
		// gap, then stop: no point scraping /graph from a venti not answering /storage.
		r.ship(ctx, []metrics.Serie{r.gauge("venti.up", now, 0)})
		return
	}
	series := []metrics.Serie{r.gauge("venti.up", now, 1)}
	if st.total > 0 {
		series = append(series, r.storageSeries(st)...)
	}
	// /graph perf counters are best-effort: they need collectstats, and we skip any
	// counter whose history is empty or whose request fails (no per-scrape warning).
	series = append(series, r.graphSeries(ctx, now)...)
	r.ship(ctx, series)
}

// ship posts series to the metrics intake, logging the outcome. Empty is a no-op.
func (r *Reporter) ship(ctx context.Context, series []metrics.Serie) {
	if len(series) == 0 {
		return
	}
	if err := metrics.SendSeries(ctx, r.client, r.endpoints, series); err != nil {
		r.log.Warn("venti series send failed", "err", err)
		return
	}
	r.log.Debug("venti metrics sent", "hostname", r.hostname, "series", len(series))
}

// gauge builds a host-tagged gauge serie with the configured tags plus any extras
// (e.g. "device:venti", which the serializer lifts into the top-level device field).
func (r *Reporter) gauge(name string, ts time.Time, v float64, extra ...string) metrics.Serie {
	tags := make([]string, 0, len(r.tags)+len(extra))
	tags = append(tags, r.tags...)
	tags = append(tags, extra...)
	return metrics.Serie{
		Name:     name,
		Type:     metrics.TypeGauge,
		Points:   []metrics.Point{{Ts: ts.Unix(), Value: v}},
		Interval: int64(r.interval / time.Second),
		Tags:     tags,
		Host:     r.hostname,
	}
}

// /storage: disk space + arenas + clumps + compression

func (r *Reporter) fetchStorage(ctx context.Context) (ventiStorage, error) {
	body, err := r.get(ctx, r.base+"/storage")
	if err != nil {
		return ventiStorage{}, err
	}
	return parseVentiStorage(body), nil
}

// storageSeries builds the disk-space gauges (system.disk.*{device:venti}, kB, to
// match the stock disk unit) plus the venti-specific gauges (arenas, clumps, data
// sizes, compression ratio, host-tagged). Caller guarantees st.total > 0.
func (r *Reporter) storageSeries(st ventiStorage) []metrics.Serie {
	const kb = 1024.0
	now := time.Now()
	var free uint64
	if st.total >= st.used {
		free = st.total - st.used
	}
	out := []metrics.Serie{
		r.gauge("system.disk.total", now, float64(st.total)/kb, "device:venti"),
		r.gauge("system.disk.used", now, float64(st.used)/kb, "device:venti"),
		r.gauge("system.disk.free", now, float64(free)/kb, "device:venti"),
		r.gauge("system.disk.in_use", now, float64(st.used)/float64(st.total), "device:venti"),
		r.gauge("venti.arenas.total", now, float64(st.arenas)),
		r.gauge("venti.arenas.active", now, float64(st.arenasActive)),
		r.gauge("venti.clumps.total", now, float64(st.clumps)),
		r.gauge("venti.clumps.compressed", now, float64(st.cclumps)),
		r.gauge("venti.data.uncompressed_bytes", now, float64(st.uncBytes)),
		r.gauge("venti.data.compressed_bytes", now, float64(st.compBytes)),
	}
	if st.compBytes > 0 {
		out = append(out, r.gauge("venti.data.compression_ratio", now, float64(st.uncBytes)/float64(st.compBytes)))
	}
	return out
}

// ventiStorage is the slice of venti's /storage page we use, in bytes/counts.
type ventiStorage struct {
	total, used          uint64 // bytes
	arenas, arenasActive uint64
	clumps, cclumps      uint64
	uncBytes, compBytes  uint64 // "data" (uncompressed) and "compressed data"
}

// parseVentiStorage reads venti's plaintext /storage page (sys/src/cmd/venti/srv/
// httpd.c sindex), whose four lines are:
//
//	index=<name>
//	total arenas=<n> active=<m>
//	total space=<size> used=<used>
//	clumps=<n> compressed clumps=<m> data=<bytes> compressed data=<bytes>
//
// Numbers use %,d/%,lld (comma thousands separators), stripped by parseCommaUint.
// We parse per token and track whether the previous token was "compressed" so the
// "compressed clumps="/"compressed data=" keys don't collide with "clumps="/"data=".
func parseVentiStorage(data string) ventiStorage {
	var st ventiStorage
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		for i, tok := range fields {
			k, v, ok := strings.Cut(tok, "=")
			if !ok {
				continue
			}
			n := parseCommaUint(v)
			compressed := i > 0 && fields[i-1] == "compressed"
			switch {
			case k == "space":
				st.total = n
			case k == "used":
				st.used = n
			case k == "arenas":
				st.arenas = n
			case k == "active":
				st.arenasActive = n
			case k == "clumps" && compressed:
				st.cclumps = n
			case k == "clumps":
				st.clumps = n
			case k == "data" && compressed:
				st.compBytes = n
			case k == "data":
				st.uncBytes = n
			}
		}
	}
	return st
}

// /graph: selected performance counters

// ventiGraphMetrics maps the venti /graph counter names we collect to their Datadog
// metric names. Each is a cumulative counter shipped as a per-second rate. venti's
// rich /stats page is commented out upstream, so /graph is the only counter path.
var ventiGraphMetrics = []struct{ stat, metric string }{
	{"rpcread", "venti.rpc.reads"},
	{"rpcwrite", "venti.rpc.writes"},
	{"rpcreadbyte", "venti.rpc.read_bytes"},
	{"rpcwritebyte", "venti.rpc.write_bytes"},
	{"rpcreadfail", "venti.rpc.read_fails"},
	{"rpcwritefail", "venti.rpc.write_fails"},
	{"icachehit", "venti.icache.hits"},
	{"icachemiss", "venti.icache.misses"},
	{"dcachehit", "venti.dcache.hits"},
	{"dcachemiss", "venti.dcache.misses"},
}

// graphSeries fetches the selected counters' current cumulative values and ships
// each as a per-second rate (the established diff-two-reads idiom). The first scrape
// only establishes the baseline. Counter resets are skipped.
func (r *Reporter) graphSeries(ctx context.Context, now time.Time) []metrics.Serie {
	cur := make(map[string]uint64, len(ventiGraphMetrics))
	for _, g := range ventiGraphMetrics {
		if v, ok := r.fetchGraph(ctx, g.stat); ok {
			cur[g.stat] = v
		}
	}
	// One debug line (not per-stat) when /graph yields nothing: venti without
	// collectstats, or an old build. /storage metrics still ship. This just explains
	// the missing venti.rpc/icache/dcache rates.
	if len(cur) == 0 {
		r.log.Debug("venti /graph stats unavailable (needs collectstats); shipping /storage metrics only")
	}
	var out []metrics.Serie
	if len(r.prev) > 0 {
		if dt := now.Sub(r.prevTs).Seconds(); dt > 0 {
			for _, g := range ventiGraphMetrics {
				c, okc := cur[g.stat]
				p, okp := r.prev[g.stat]
				if okc && okp && c >= p {
					out = append(out, r.gauge(g.metric, now, float64(c-p)/dt))
				}
			}
		}
	}
	r.prev = cur
	r.prevTs = now
	return out
}

func (r *Reporter) fetchGraph(ctx context.Context, stat string) (uint64, bool) {
	// raw graph of the last 30s, the bins carry the cumulative counter's samples.
	u := r.base + "/graph?arg=" + url.QueryEscape(stat) + "&graph=raw&text=1&t0=-30&t1=0"
	body, err := r.get(ctx, u)
	if err != nil {
		return 0, false
	}
	return parseGraphRaw(body)
}

// parseGraphRaw parses venti's /graph?...&text=1 output (httpd.c dotextbin): a
// "stats" header then lines "i: nsamp=N min=X max=Y avg=Z". For a cumulative counter
// the current value is the largest max among bins that actually carry a sample,
// ok=false when no bin has nsamp>0 (collectstats off, or no history yet).
func parseGraphRaw(data string) (uint64, bool) {
	var max uint64
	var found bool
	for _, line := range strings.Split(data, "\n") {
		var nsamp int64 = -1
		var m uint64
		var haveMax bool
		for _, tok := range strings.Fields(line) {
			k, v, ok := strings.Cut(tok, "=")
			if !ok {
				continue
			}
			switch k {
			case "nsamp":
				nsamp, _ = strconv.ParseInt(v, 10, 64)
			case "max":
				// venti prints these counters from 32-bit uints through %d (httpd.c
				// dotextbin), so a value in [2^31,2^32) prints negative and decodes
				// as the wrapped 32-bit value. A real wrap past 2^32 reads as a
				// counter reset, which graphSeries skips.
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					if n < 0 {
						n = int64(uint32(int32(n)))
					}
					m, haveMax = uint64(n), true
				}
			}
		}
		if nsamp > 0 && haveMax {
			found = true
			if m > max {
				max = m
			}
		}
	}
	return max, found
}

// helpers

func (r *Reporter) get(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func parseCommaUint(s string) uint64 {
	v, _ := strconv.ParseUint(strings.ReplaceAll(s, ",", ""), 10, 64)
	return v
}
