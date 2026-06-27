package metrics

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

// Aggregator owns the DogStatsD metric state. A single goroutine (Run) drains
// the sample channel and, on a ticker, flushes the accumulated contexts plus the
// pulled host series to the intake. Single ownership means no locks: the maps
// are touched only from Run. Listeners hand samples in with Add, which never
// blocks. At this scale a brief flush shouldn't stall ingestion, and dropping a
// sample under pressure is the right DogStatsD behavior.
type Aggregator struct {
	interval       time.Duration
	intervalS      int64
	hostname       string
	tags           []string
	sources        []SeriesSource
	client         *intake.Client
	endpoints      []intake.Endpoint // series intake (primary + additional)
	checkEndpoints []intake.Endpoint // service-check intake
	eventEndpoints []intake.Endpoint // events intake (/intake/, key carried in the body)
	log            *slog.Logger

	samples  chan Sample
	checks   chan ServiceCheck
	events   chan Event
	dropped  atomic.Int64
	contexts map[string]*aggContext

	// Owned by Run: service checks and events arrive on their channels and batch here
	// until the next flush. They aren't aggregated, just forwarded.
	pendingChecks []ServiceCheck
	pendingEvents []Event
}

// aggContext is the running aggregation for one (name, type, tags) tuple.
type aggContext struct {
	name    string
	typ     SampleType
	tags    []string
	value   float64             // gauge: last value, counter: running sum
	samples []histSample        // histogram/timing: observed (value, weight) pairs
	count   float64             // histogram/timing: sum of weights (rate-corrected count)
	members map[string]struct{} // set: distinct members
}

// histSample is one observed value of a histogram/timing metric with its weight
// (1/sample_rate), so a sampled client submission counts for the whole it represents.
type histSample struct {
	value, weight float64
}

// AggregatorOptions configures an Aggregator.
type AggregatorOptions struct {
	Interval          time.Duration // flush period, defaults to 15s
	Hostname          string        // attached to every serie lacking its own host
	Tags              []string      // global tags appended to every serie
	Sources           []SeriesSource
	Client            *intake.Client
	Endpoints         []intake.Endpoint // series intake (primary + additional)
	CheckRunEndpoints []intake.Endpoint // service-check intake. Checks dropped if empty
	EventsEndpoints   []intake.Endpoint // events intake (/intake/). Events dropped if empty
	Logger            *slog.Logger
	BufferSize        int // sample channel capacity, defaults to 8192
}

// NewAggregator returns an Aggregator. Call Run in its own goroutine.
func NewAggregator(o AggregatorOptions) *Aggregator {
	if o.Interval == 0 {
		o.Interval = 15 * time.Second
	}
	if o.BufferSize == 0 {
		o.BufferSize = 8192
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	intervalS := int64(o.Interval / time.Second)
	if intervalS < 1 {
		intervalS = 1
	}
	return &Aggregator{
		interval:       o.Interval,
		intervalS:      intervalS,
		hostname:       o.Hostname,
		tags:           o.Tags,
		sources:        o.Sources,
		client:         o.Client,
		endpoints:      o.Endpoints,
		checkEndpoints: o.CheckRunEndpoints,
		eventEndpoints: o.EventsEndpoints,
		log:            o.Logger,
		samples:        make(chan Sample, o.BufferSize),
		checks:         make(chan ServiceCheck, 1024),
		events:         make(chan Event, 1024),
		contexts:       make(map[string]*aggContext),
	}
}

// Add hands a sample to the aggregator without blocking. If the buffer is full
// the sample is dropped and counted.
func (a *Aggregator) Add(s Sample) {
	select {
	case a.samples <- s:
	default:
		a.dropped.Add(1)
	}
}

// AddServiceCheck queues a service check (non-blocking, dropped under pressure).
func (a *Aggregator) AddServiceCheck(sc ServiceCheck) {
	select {
	case a.checks <- sc:
	default:
		a.dropped.Add(1)
	}
}

// AddEvent queues an event (non-blocking, dropped under pressure).
func (a *Aggregator) AddEvent(e Event) {
	select {
	case a.events <- e:
	default:
		a.dropped.Add(1)
	}
}

// Run drains samples and flushes on the interval until ctx is cancelled, then
// drains whatever Add already accepted and flushes once more, so the final
// interval isn't lost.
func (a *Aggregator) Run(ctx context.Context) {
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.drain()
			a.flush(time.Now())
			return
		case s := <-a.samples:
			a.accumulate(s)
		case sc := <-a.checks:
			a.pendingChecks = append(a.pendingChecks, sc)
		case e := <-a.events:
			a.pendingEvents = append(a.pendingEvents, e)
		case <-ticker.C:
			a.flush(time.Now())
			if n := a.dropped.Swap(0); n > 0 {
				a.log.Debug("dropped dogstatsd samples, checks, or events (buffers full)", "count", n)
			}
		}
	}
}

// drain empties what Add queued but Run has not yet consumed, so the final flush
// covers everything accepted before shutdown.
func (a *Aggregator) drain() {
	for {
		select {
		case s := <-a.samples:
			a.accumulate(s)
		case sc := <-a.checks:
			a.pendingChecks = append(a.pendingChecks, sc)
		case e := <-a.events:
			a.pendingEvents = append(a.pendingEvents, e)
		default:
			return
		}
	}
}

func (a *Aggregator) accumulate(s Sample) {
	rate := s.SampleRate
	if rate <= 0 {
		rate = 1
	}
	var value, weight float64 // the sample's numeric contribution, checked below
	switch s.Type {
	case SampleGauge:
		value = s.Value
	case SampleCounter:
		value = s.Value / rate
	case SampleHistogram, SampleTiming:
		value, weight = s.Value, 1/rate
	case SampleSet:
		// the member is a string, nothing numeric to check
	default:
		return // distribution needs DDSketch (protobuf), excluded by charter, dropped
	}
	if !finite(value) || !finite(weight) {
		return // one NaN or Inf point would make the whole series payload unmarshalable
	}

	key := contextKey(s.Name, s.Type, s.Tags)
	c := a.contexts[key]
	if c == nil {
		c = &aggContext{name: s.Name, typ: s.Type, tags: append([]string(nil), s.Tags...)}
		a.contexts[key] = c
	}
	switch s.Type {
	case SampleGauge:
		c.value = value
	case SampleCounter:
		c.value += value
	case SampleHistogram, SampleTiming:
		c.samples = append(c.samples, histSample{value: value, weight: weight})
		c.count += weight
	case SampleSet:
		if c.members == nil {
			c.members = make(map[string]struct{})
		}
		c.members[s.Str] = struct{}{}
	}
}

// collect turns the accumulated state into series, resets it, appends the host
// series, and finalizes host + global tags on everything. It is the testable
// core of flush.
func (a *Aggregator) collect(now time.Time) []Serie {
	ts := now.Unix()
	series := make([]Serie, 0, len(a.contexts))
	for _, c := range a.contexts {
		series = append(series, c.toSeries(ts, a.intervalS)...)
	}
	clear(a.contexts)

	for _, src := range a.sources {
		batch := src.Collect()
		for i := range batch {
			if batch[i].SourceTypeName == "" {
				batch[i].SourceTypeName = "System" // the stock Agent stamps check metrics
			}
		}
		series = append(series, batch...)
	}

	for i := range series {
		if series[i].Host == "" {
			series[i].Host = a.hostname
		}
		if series[i].Interval == 0 {
			series[i].Interval = a.intervalS
		}
		series[i].Tags = appendTags(series[i].Tags, a.tags)
	}
	return series
}

func (a *Aggregator) flush(now time.Time) {
	// A fresh context so the final flush still sends after Run's ctx is done.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if series := a.collect(now); len(series) > 0 {
		if err := SendSeries(ctx, a.client, a.endpoints, series); err != nil {
			a.log.Warn("metrics flush failed", "series", len(series), "err", err)
		} else {
			a.log.Debug("metrics flushed", "series", len(series))
		}
	}
	a.flushChecks(ctx, now)
	a.flushEvents(ctx, now)
}

// flushChecks finalizes (host/timestamp/global tags) and ships any queued service
// checks. They aren't aggregated, just batched into one /api/v1/check_run array.
func (a *Aggregator) flushChecks(ctx context.Context, now time.Time) {
	if len(a.pendingChecks) == 0 {
		return
	}
	checks := a.pendingChecks
	a.pendingChecks = nil
	if len(a.checkEndpoints) == 0 {
		return
	}
	ts := now.Unix()
	for i := range checks {
		if checks[i].Host == "" {
			checks[i].Host = a.hostname
		}
		if checks[i].Timestamp == 0 {
			checks[i].Timestamp = ts
		}
		checks[i].Tags = appendTags(checks[i].Tags, a.tags)
	}
	if err := SendServiceChecks(ctx, a.client, a.checkEndpoints, checks); err != nil {
		a.log.Warn("service checks flush failed", "checks", len(checks), "err", err)
	} else {
		a.log.Debug("service checks sent", "checks", len(checks))
	}
}

// flushEvents finalizes (host/date/global tags) and ships any queued events.
func (a *Aggregator) flushEvents(ctx context.Context, now time.Time) {
	if len(a.pendingEvents) == 0 {
		return
	}
	events := a.pendingEvents
	a.pendingEvents = nil
	if len(a.eventEndpoints) == 0 {
		return
	}
	ts := now.Unix()
	for i := range events {
		if events[i].Host == "" {
			events[i].Host = a.hostname
		}
		if events[i].Timestamp == 0 {
			events[i].Timestamp = ts
		}
		events[i].Tags = appendTags(events[i].Tags, a.tags)
	}
	if err := SendEvents(ctx, a.client, a.eventEndpoints, a.hostname, events); err != nil {
		a.log.Warn("events flush failed", "events", len(events), "err", err)
	} else {
		a.log.Debug("events sent", "events", len(events))
	}
}

// toSeries renders a context's accumulated state into one or more series. Gauge/counter
// yield one. A histogram/timing expands to count/avg/median/max/95percentile (the stock
// Agent's default aggregates). A set yields its distinct-member count.
func (c *aggContext) toSeries(ts, intervalS int64) []Serie {
	mk := func(name string, typ SerieType, v float64) Serie {
		return Serie{Name: name, Type: typ, Tags: c.tags, Interval: intervalS, Points: []Point{{Ts: ts, Value: v}}}
	}
	switch c.typ {
	case SampleCounter:
		return []Serie{mk(c.name, TypeRate, c.value/float64(intervalS))}
	case SampleHistogram, SampleTiming:
		return c.histogramSeries(ts, intervalS, mk)
	case SampleSet:
		return []Serie{mk(c.name, TypeGauge, float64(len(c.members)))}
	default: // SampleGauge
		return []Serie{mk(c.name, TypeGauge, c.value)}
	}
}

// histogramSeries expands a histogram/timing context into the stock default aggregates:
// .count (a rate, like a counter), .avg, .median, .max, and .95percentile (gauges). The
// median/percentile use the Agent's weighted-cumulative method (pkg/metrics/histogram.go),
// so sampled submissions (weight 1/rate) are honored.
func (c *aggContext) histogramSeries(ts, intervalS int64, mk func(string, SerieType, float64) Serie) []Serie {
	if len(c.samples) == 0 {
		return nil
	}
	sort.Slice(c.samples, func(i, j int) bool { return c.samples[i].value < c.samples[j].value })
	var sum float64
	for _, s := range c.samples {
		sum += s.value * s.weight
	}
	return []Serie{
		mk(c.name+".count", TypeRate, c.count/float64(intervalS)),
		mk(c.name+".avg", TypeGauge, sum/c.count),
		mk(c.name+".median", TypeGauge, c.weightedQuantile((c.count-1)/2)),
		mk(c.name+".max", TypeGauge, c.samples[len(c.samples)-1].value),
		mk(c.name+".95percentile", TypeGauge, c.weightedQuantile((95*c.count-1)/100)),
	}
}

// weightedQuantile returns the value of the first sample (sorted ascending) whose
// cumulative weight exceeds target, the stock Agent's histogram quantile method.
func (c *aggContext) weightedQuantile(target float64) float64 {
	var weight float64
	for _, s := range c.samples {
		weight += s.weight
		if weight > target {
			return s.value
		}
	}
	return c.samples[len(c.samples)-1].value
}

// contextKey identifies a context by name, type, and tag set. Tags are sorted so
// that order on the wire doesn't fragment a metric across contexts.
func contextKey(name string, typ SampleType, tags []string) string {
	sorted := append([]string(nil), tags...)
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('|')
	b.WriteByte('0' + byte(typ))
	b.WriteByte('|')
	for i, t := range sorted {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(t)
	}
	return b.String()
}

func appendTags(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	return append(out, b...)
}
