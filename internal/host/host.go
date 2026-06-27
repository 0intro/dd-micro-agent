// Package host collects host system metrics (cpu, memory, load, uptime,
// filesystem, network) and reports them as gauges, mirroring the stock Agent's
// core checks. The OS-independent plumbing lives here. Per-OS collection lives in
// the build-tagged files, each providing collectors(). Counters (cpu jiffies, net
// bytes) become per-second or percentage gauges by diffing consecutive reads.
package host

import (
	"log/slog"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// Collector gathers host metrics. Construct it with New and call Collect once per
// interval. It satisfies metrics.SeriesSource.
type Collector struct {
	proc     string // /proc root (Linux only)
	interval int64
	log      *slog.Logger
	subs     []subCollector
}

// Options configures a Collector.
type Options struct {
	Proc     string // proc root (Linux), defaults to "/proc"
	Interval int64  // seconds, defaults to 15
	Logger   *slog.Logger
}

// subCollector is one metric family. Collectors that turn counters into rates
// keep their previous reading as state, so collect is not safe for concurrent
// use. The aggregator calls it from a single goroutine.
type subCollector interface {
	name() string
	collect(now time.Time) ([]metrics.Serie, error)
}

// New returns a Collector wired with the sub-collectors available on this OS.
func New(opts Options) *Collector {
	if opts.Proc == "" {
		opts.Proc = "/proc"
	}
	if opts.Interval == 0 {
		opts.Interval = 15
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	c := &Collector{proc: opts.Proc, interval: opts.Interval, log: opts.Logger}
	c.subs = collectors(c)
	return c
}

// Collect reads every sub-collector and returns their series tagged with the
// collection interval. A failing sub-collector is logged and skipped, not fatal.
func (c *Collector) Collect() []metrics.Serie {
	now := time.Now()
	var out []metrics.Serie
	for _, sub := range c.subs {
		series, err := sub.collect(now)
		if err != nil {
			c.log.Warn("host collector failed", "collector", sub.name(), "err", err)
			continue
		}
		for i := range series {
			series[i].Interval = c.interval
		}
		out = append(out, series...)
	}
	return out
}

// gauge builds a single-point gauge serie at ts. Interval is filled in by Collect.
func gauge(name string, ts time.Time, value float64, tags ...string) metrics.Serie {
	return metrics.Serie{
		Name:   name,
		Type:   metrics.TypeGauge,
		Points: []metrics.Point{{Ts: ts.Unix(), Value: value}},
		Tags:   tags,
	}
}
