package dogstatsd

import (
	"reflect"
	"testing"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

func TestParse(t *testing.T) {
	one := func(s metrics.Sample) []metrics.Sample { return []metrics.Sample{s} }
	tests := []struct {
		name string
		line string
		want []metrics.Sample
		ok   bool
	}{
		{"gauge", "page.views:42|g",
			one(metrics.Sample{Name: "page.views", Value: 42, Type: metrics.SampleGauge, SampleRate: 1}), true},
		{"counter with tags", "hits:1|c|#env:prod,team:x",
			one(metrics.Sample{Name: "hits", Value: 1, Type: metrics.SampleCounter, SampleRate: 1, Tags: []string{"env:prod", "team:x"}}), true},
		{"counter sample rate", "hits:7|c|@0.5",
			one(metrics.Sample{Name: "hits", Value: 7, Type: metrics.SampleCounter, SampleRate: 0.5}), true},
		{"timing", "latency:250|ms",
			one(metrics.Sample{Name: "latency", Value: 250, Type: metrics.SampleTiming, SampleRate: 1}), true},
		{"histogram", "render:3|h",
			one(metrics.Sample{Name: "render", Value: 3, Type: metrics.SampleHistogram, SampleRate: 1}), true},
		{"distribution", "size:3|d",
			one(metrics.Sample{Name: "size", Value: 3, Type: metrics.SampleDistribution, SampleRate: 1}), true},
		{"set captures member string", "users:bob|s",
			one(metrics.Sample{Name: "users", Str: "bob", Type: metrics.SampleSet, SampleRate: 1}), true},
		{"set numeric member kept as string", "users:42|s",
			one(metrics.Sample{Name: "users", Str: "42", Type: metrics.SampleSet, SampleRate: 1}), true},
		{"set member may contain colons", "users:a:b|s",
			one(metrics.Sample{Name: "users", Str: "a:b", Type: metrics.SampleSet, SampleRate: 1}), true},
		{"rate then tags", "m:2|c|@0.25|#a:b",
			one(metrics.Sample{Name: "m", Value: 2, Type: metrics.SampleCounter, SampleRate: 0.25, Tags: []string{"a:b"}}), true},
		{"ignores container id and timestamp", "m:1|g|#a|c:abc123|T1700000000",
			one(metrics.Sample{Name: "m", Value: 1, Type: metrics.SampleGauge, SampleRate: 1, Tags: []string{"a"}}), true},
		{"packed values share type, rate, and tags", "m:1:2:3|h|@0.5|#a",
			[]metrics.Sample{
				{Name: "m", Value: 1, Type: metrics.SampleHistogram, SampleRate: 0.5, Tags: []string{"a"}},
				{Name: "m", Value: 2, Type: metrics.SampleHistogram, SampleRate: 0.5, Tags: []string{"a"}},
				{Name: "m", Value: 3, Type: metrics.SampleHistogram, SampleRate: 0.5, Tags: []string{"a"}},
			}, true},

		{"no type", "m:1", nil, false},
		{"no value", "m|g", nil, false},
		{"empty name", ":1|g", nil, false},
		{"bad value", "m:abc|g", nil, false},
		{"one bad packed value spoils the line", "m:1:x:3|h", nil, false},
		{"unknown type", "m:1|x", nil, false},
		{"empty line", "", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parse(tt.line)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestParseServiceCheck(t *testing.T) {
	tests := []struct {
		name string
		line string
		want metrics.ServiceCheck
		ok   bool
	}{
		{"minimal", "_sc|app.up|0",
			metrics.ServiceCheck{Check: "app.up", Status: metrics.ServiceOK}, true},
		{"full", "_sc|app.up|2|d:100|h:web1|#env:prod,role:api|m:it is down",
			metrics.ServiceCheck{Check: "app.up", Status: metrics.ServiceCritical, Timestamp: 100, Host: "web1", Tags: []string{"env:prod", "role:api"}, Message: "it is down"}, true},
		{"message keeps pipes", "_sc|c|1|m:a|b",
			metrics.ServiceCheck{Check: "c", Status: metrics.ServiceWarning, Message: "a|b"}, true},
		{"bad status", "_sc|c|9", metrics.ServiceCheck{}, false},
		{"no status", "_sc|c", metrics.ServiceCheck{}, false},
		{"not a service check", "foo:1|g", metrics.ServiceCheck{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseServiceCheck(tt.line)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// The fuzz targets guard the one property the read loop depends on: no datagram,
// however hostile, may panic a parser. Without -fuzz they still run the seeds.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{"page.views:42|g", "m:1:2:3|h|@0.5|#a:b", "users:a:b|s", "m:nan|g", ":1|g", "m:|g", "m:1|x|", "m:1|g|#", "m:1|g|@"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) { parse(line) })
}

func FuzzParseServiceCheck(f *testing.F) {
	for _, seed := range []string{"_sc|app.up|0", "_sc|c|1|m:a|b", "_sc|c|9", "_sc|", "_sc||0", "_sc|c|0|d:|h:|#|m:"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) { parseServiceCheck(line) })
}

func FuzzParseEvent(f *testing.F) {
	for _, seed := range []string{"_e{5,5}:hello|world", "_e{1,3}:t|a|b", "_e{50,5}:hi|there", "_e{,}:|", "_e{1,1}:", "_e{-1,-1}:x|y"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) { parseEvent(line) })
}

func TestParseEvent(t *testing.T) {
	tests := []struct {
		name string
		line string
		want metrics.Event
		ok   bool
	}{
		{"minimal", "_e{5,5}:hello|world",
			metrics.Event{Title: "hello", Text: "world"}, true},
		{"full", "_e{3,4}:dep|done|d:200|h:h1|p:low|t:success|s:my|k:agg|#a:b",
			metrics.Event{Title: "dep", Text: "done", Timestamp: 200, Host: "h1", Priority: "low", AlertType: "success", SourceTypeName: "my", AggregationKey: "agg", Tags: []string{"a:b"}}, true},
		{"newline escape in text", "_e{1,6}:t|a\\nb c",
			metrics.Event{Title: "t", Text: "a\nb c"}, true},
		{"newline escape in title", "_e{4,1}:a\\nb|x",
			metrics.Event{Title: "a\nb", Text: "x"}, true},
		{"pipe in text via length", "_e{1,3}:t|a|b",
			metrics.Event{Title: "t", Text: "a|b"}, true},
		{"bad header", "_e{x,5}:hello|world", metrics.Event{}, false},
		{"length overflow", "_e{50,5}:hi|there", metrics.Event{}, false},
		{"empty title rejected", "_e{0,3}:|abc", metrics.Event{}, false},
		{"not an event", "foo:1|g", metrics.Event{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseEvent(tt.line)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}
