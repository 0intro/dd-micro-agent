package metrics

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

func gaugeSample(name string, v float64, tags ...string) Sample {
	return Sample{Name: name, Value: v, Type: SampleGauge, SampleRate: 1, Tags: tags}
}

func counterSample(name string, v, rate float64) Sample {
	return Sample{Name: name, Value: v, Type: SampleCounter, SampleRate: rate}
}

func newAgg(o AggregatorOptions) *Aggregator {
	if o.Interval == 0 {
		o.Interval = 15 * time.Second
	}
	return NewAggregator(o)
}

func byName(series []Serie) map[string]Serie {
	m := make(map[string]Serie, len(series))
	for _, s := range series {
		m[s.Name] = s
	}
	return m
}

func TestAggregatorGaugeLastValueWins(t *testing.T) {
	a := newAgg(AggregatorOptions{Hostname: "h1"})
	a.accumulate(gaugeSample("g", 1))
	a.accumulate(gaugeSample("g", 5))

	series := a.collect(time.Unix(100, 0))
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	s := series[0]
	if s.Type != TypeGauge || s.Points[0].Value != 5 || s.Points[0].Ts != 100 {
		t.Errorf("gauge serie = %+v", s)
	}
	if s.Host != "h1" || s.Interval != 15 {
		t.Errorf("host/interval not injected: %+v", s)
	}
}

func TestAggregatorCounterBecomesRate(t *testing.T) {
	a := newAgg(AggregatorOptions{})
	a.accumulate(counterSample("c", 30, 1))
	a.accumulate(counterSample("c", 30, 1)) // sum = 60 over the interval

	s := a.collect(time.Unix(0, 0))[0]
	if s.Type != TypeRate {
		t.Errorf("type = %v, want rate", s.Type)
	}
	if got := s.Points[0].Value; got != 4 { // 60 / 15
		t.Errorf("rate value = %v, want 4", got)
	}
	if got := s.Points[0].Value * float64(s.Interval); got != 60 {
		t.Errorf("rate*interval = %v, want 60 (the original count)", got)
	}
}

func TestAggregatorCounterSampleRateScalesUp(t *testing.T) {
	a := newAgg(AggregatorOptions{})
	a.accumulate(counterSample("c", 1, 0.1)) // one in ten => counts as 10

	s := a.collect(time.Unix(0, 0))[0]
	if got := s.Points[0].Value * float64(s.Interval); got != 10 {
		t.Errorf("scaled count = %v, want 10", got)
	}
}

func TestAggregatorContextIgnoresTagOrder(t *testing.T) {
	a := newAgg(AggregatorOptions{})
	a.accumulate(gaugeSample("g", 1, "a:1", "b:2"))
	a.accumulate(gaugeSample("g", 7, "b:2", "a:1")) // same context

	series := a.collect(time.Unix(0, 0))
	if len(series) != 1 {
		t.Fatalf("tag order fragmented context into %d series", len(series))
	}
	if series[0].Points[0].Value != 7 {
		t.Errorf("value = %v, want 7", series[0].Points[0].Value)
	}
}

func TestAggregatorDropsDistribution(t *testing.T) {
	a := newAgg(AggregatorOptions{})
	a.accumulate(Sample{Name: "x", Value: 1, Type: SampleDistribution, SampleRate: 1})
	if series := a.collect(time.Unix(0, 0)); len(series) != 0 {
		t.Errorf("got %d series, want 0 (distribution dropped, needs DDSketch/protobuf)", len(series))
	}
}

func TestAggregatorHistogram(t *testing.T) {
	a := newAgg(AggregatorOptions{})
	for i := 1; i <= 10; i++ { // values 1..10, weight 1 each
		a.accumulate(Sample{Name: "h", Value: float64(i), Type: SampleHistogram, SampleRate: 1})
	}
	m := byName(a.collect(time.Unix(0, 0)))
	if len(m) != 5 {
		t.Fatalf("got %d series, want 5 (count/avg/median/max/95percentile)", len(m))
	}
	// .count is a rate (count/interval). Count over 1..10 is 10.
	if s := m["h.count"]; s.Type != TypeRate || s.Points[0].Value*float64(s.Interval) != 10 {
		t.Errorf("h.count = %+v, want rate carrying count 10", s)
	}
	for name, want := range map[string]float64{
		"h.avg": 5.5, "h.median": 5, "h.max": 10, "h.95percentile": 10,
	} {
		if s := m[name]; s.Type != TypeGauge || s.Points[0].Value != want {
			t.Errorf("%s = %+v, want gauge %v", name, s, want)
		}
	}
}

func TestAggregatorHistogramSampleRate(t *testing.T) {
	a := newAgg(AggregatorOptions{})
	a.accumulate(Sample{Name: "h", Value: 1, Type: SampleHistogram, SampleRate: 0.5}) // weight 2
	a.accumulate(Sample{Name: "h", Value: 4, Type: SampleHistogram, SampleRate: 1})   // weight 1
	m := byName(a.collect(time.Unix(0, 0)))
	if got := m["h.count"].Points[0].Value * float64(m["h.count"].Interval); got != 3 {
		t.Errorf("h.count = %v, want 3 (weights 2+1)", got)
	}
	if got := m["h.avg"].Points[0].Value; got != 2 { // (1*2 + 4*1)/3 = 2
		t.Errorf("h.avg = %v, want 2 (weighted)", got)
	}
}

func TestAggregatorTimingExpands(t *testing.T) {
	a := newAgg(AggregatorOptions{})
	a.accumulate(Sample{Name: "t", Value: 100, Type: SampleTiming, SampleRate: 1})
	m := byName(a.collect(time.Unix(0, 0)))
	if m["t.max"].Points[0].Value != 100 || m["t.avg"].Points[0].Value != 100 {
		t.Errorf("timing did not expand like a histogram: %+v", m)
	}
}

func TestAggregatorSetCountsDistinct(t *testing.T) {
	a := newAgg(AggregatorOptions{})
	for _, u := range []string{"alice", "bob", "alice", "carol"} { // 3 distinct
		a.accumulate(Sample{Name: "users", Str: u, Type: SampleSet, SampleRate: 1})
	}
	series := a.collect(time.Unix(0, 0))
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	if s := series[0]; s.Name != "users" || s.Type != TypeGauge || s.Points[0].Value != 3 {
		t.Errorf("set serie = %+v, want users gauge 3", s)
	}
}

// One NaN or Inf point makes the whole JSON payload unmarshalable, so
// non-finite samples are dropped at the door, whatever produced them (a hostile
// datagram value or a denormal sample rate whose reciprocal overflows).
func TestAggregatorDropsNonFiniteSamples(t *testing.T) {
	a := newAgg(AggregatorOptions{})
	a.accumulate(Sample{Name: "g", Value: math.NaN(), Type: SampleGauge, SampleRate: 1})
	a.accumulate(Sample{Name: "g", Value: math.Inf(1), Type: SampleGauge, SampleRate: 1})
	a.accumulate(counterSample("c", math.Inf(-1), 1))
	a.accumulate(Sample{Name: "h", Value: 1, Type: SampleHistogram, SampleRate: 1e-323}) // Inf weight
	if series := a.collect(time.Unix(0, 0)); len(series) != 0 {
		t.Errorf("got %d series, want 0 (non-finite samples dropped)", len(series))
	}

	a.accumulate(gaugeSample("g", 2))
	if series := a.collect(time.Unix(0, 0)); len(series) != 1 || series[0].Points[0].Value != 2 {
		t.Errorf("finite gauge lost after the drops: %+v", series)
	}
}

// The cumulative-weight walk must honor non-uniform weights: with (1, weight 2)
// and (4, weight 1), the median target (count-1)/2 = 1 falls inside the 1s.
func TestAggregatorWeightedQuantiles(t *testing.T) {
	a := newAgg(AggregatorOptions{})
	a.accumulate(Sample{Name: "h", Value: 1, Type: SampleHistogram, SampleRate: 0.5}) // weight 2
	a.accumulate(Sample{Name: "h", Value: 4, Type: SampleHistogram, SampleRate: 1})   // weight 1
	m := byName(a.collect(time.Unix(0, 0)))
	if got := m["h.median"].Points[0].Value; got != 1 {
		t.Errorf("weighted median = %v, want 1", got)
	}
	if got := m["h.95percentile"].Points[0].Value; got != 4 { // target 2.84 crosses into the 4
		t.Errorf("weighted 95percentile = %v, want 4", got)
	}
}

func TestAggregatorSingleSampleQuantiles(t *testing.T) {
	a := newAgg(AggregatorOptions{})
	a.accumulate(Sample{Name: "h", Value: 7, Type: SampleHistogram, SampleRate: 1})
	m := byName(a.collect(time.Unix(0, 0)))
	for _, name := range []string{"h.avg", "h.median", "h.max", "h.95percentile"} {
		if got := m[name].Points[0].Value; got != 7 {
			t.Errorf("%s = %v, want 7 (single sample)", name, got)
		}
	}
}

type fakeSource struct{ series []Serie }

func (f fakeSource) Collect() []Serie { return f.series }

// Source series are check metrics in the stock Agent's sense, so collect stamps
// them source_type_name System. DogStatsD series stay unstamped, as in the stock.
func TestCollectStampsSourceTypeName(t *testing.T) {
	host := fakeSource{series: []Serie{{Name: "system.cpu.idle", Type: TypeGauge, Points: []Point{{Ts: 1, Value: 1}}}}}
	a := newAgg(AggregatorOptions{Sources: []SeriesSource{host}})
	a.accumulate(gaugeSample("custom.g", 3))

	m := byName(a.collect(time.Unix(0, 0)))
	if got := m["system.cpu.idle"].SourceTypeName; got != "System" {
		t.Errorf("host serie source_type_name = %q, want System", got)
	}
	if got := m["custom.g"].SourceTypeName; got != "" {
		t.Errorf("dogstatsd serie source_type_name = %q, want empty", got)
	}
}

// Run must flush on the ticker and, at shutdown, first drain what Add already
// accepted so the final interval is not lost.
func TestAggregatorRunFlushesAndDrains(t *testing.T) {
	var (
		mu    sync.Mutex
		names []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("not gzip: %v", err)
			return
		}
		body, _ := io.ReadAll(gr)
		var payload struct {
			Series []struct {
				Metric string `json:"metric"`
			} `json:"series"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("bad json: %v", err)
		}
		mu.Lock()
		for _, s := range payload.Series {
			names = append(names, s.Metric)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	a := NewAggregator(AggregatorOptions{
		Interval:  20 * time.Millisecond,
		Client:    intake.New(intake.Options{}),
		Endpoints: []intake.Endpoint{{URL: srv.URL + "/api/v1/series", APIKey: "k"}},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()

	a.Add(gaugeSample("ticked", 1))
	seen := func(name string) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(names, name)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !seen("ticked") {
		if time.Now().After(deadline) {
			t.Fatal("the periodic flush never delivered the sample")
		}
		time.Sleep(5 * time.Millisecond)
	}

	a.Add(gaugeSample("final", 1)) // accepted just before shutdown
	cancel()
	<-done
	if !seen("final") {
		t.Error("the final flush lost a sample Add had already accepted")
	}
}

func TestAggregatorMergesHostSourcesAndInjectsTags(t *testing.T) {
	host := fakeSource{series: []Serie{{
		Name:   "system.cpu.user",
		Type:   TypeGauge,
		Points: []Point{{Ts: 1, Value: 12}},
		Tags:   []string{"device:eth0"},
	}}}
	a := newAgg(AggregatorOptions{
		Hostname: "h1",
		Tags:     []string{"env:prod"},
		Sources:  []SeriesSource{host},
	})
	a.accumulate(gaugeSample("custom.g", 3, "scope:app"))

	m := byName(a.collect(time.Unix(50, 0)))
	if len(m) != 2 {
		t.Fatalf("got %d series, want 2 (dogstatsd + host)", len(m))
	}

	dsd := m["custom.g"]
	if dsd.Host != "h1" || !hasTag(dsd.Tags, "scope:app") || !hasTag(dsd.Tags, "env:prod") {
		t.Errorf("dogstatsd serie not finalized: %+v", dsd)
	}

	hostSerie := m["system.cpu.user"]
	if hostSerie.Host != "h1" || hostSerie.Interval != 15 {
		t.Errorf("host serie not finalized: %+v", hostSerie)
	}
	if !hasTag(hostSerie.Tags, "device:eth0") || !hasTag(hostSerie.Tags, "env:prod") {
		t.Errorf("host serie tags wrong: %+v", hostSerie.Tags)
	}
}

func TestCollectResetsContexts(t *testing.T) {
	a := newAgg(AggregatorOptions{})
	a.accumulate(gaugeSample("g", 1))
	if len(a.collect(time.Unix(0, 0))) != 1 {
		t.Fatal("first collect should emit the gauge")
	}
	if len(a.collect(time.Unix(0, 0))) != 0 {
		t.Error("second collect should be empty; contexts not reset")
	}
}

func TestHeartbeatSource(t *testing.T) {
	s := HeartbeatSource{}.Collect()
	if len(s) != 1 {
		t.Fatalf("got %d series, want 1", len(s))
	}
	if s[0].Name != "datadog.agent.running" || s[0].Type != TypeGauge || s[0].Points[0].Value != 1 {
		t.Errorf("heartbeat = %+v, want datadog.agent.running gauge 1", s[0])
	}
	if !hasTag(s[0].Tags, "version:"+intake.Version) {
		t.Errorf("heartbeat tags = %v, want the version tag the stock Agent sends", s[0].Tags)
	}
}

func TestHeartbeatFlowsThroughCollect(t *testing.T) {
	a := newAgg(AggregatorOptions{Hostname: "h1", Tags: []string{"env:prod"}, Sources: []SeriesSource{HeartbeatSource{}}})
	hb, ok := byName(a.collect(time.Unix(0, 0)))["datadog.agent.running"]
	if !ok {
		t.Fatal("heartbeat not emitted")
	}
	if hb.Host != "h1" || hb.Interval != 15 || !hasTag(hb.Tags, "env:prod") {
		t.Errorf("heartbeat not finalized (host/interval/tags): %+v", hb)
	}
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
