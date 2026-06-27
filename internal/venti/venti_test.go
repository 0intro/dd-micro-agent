package venti

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

func TestParseVentiStorage(t *testing.T) {
	// venti's /storage page (sindex), numbers printed with %,lld comma separators.
	data := "index=main\n" +
		"total arenas=12 active=8\n" +
		"total space=1,073,741,824 used=536,870,912\n" +
		"clumps=1,000 compressed clumps=900 data=1,200,000,000 compressed data=500,000,000\n"
	st := parseVentiStorage(data)
	if st.total != 1073741824 || st.used != 536870912 {
		t.Errorf("total/used = %d/%d, want 1073741824/536870912", st.total, st.used)
	}
	if st.arenas != 12 || st.arenasActive != 8 {
		t.Errorf("arenas = %d/%d, want 12/8", st.arenas, st.arenasActive)
	}
	// the "compressed clumps"/"compressed data" tokens must not collide with clumps/data
	if st.clumps != 1000 || st.cclumps != 900 {
		t.Errorf("clumps = %d/%d, want 1000/900", st.clumps, st.cclumps)
	}
	if st.uncBytes != 1200000000 || st.compBytes != 500000000 {
		t.Errorf("data = %d/%d, want 1200000000/500000000", st.uncBytes, st.compBytes)
	}

	// no /storage body -> zero (so the reporter ships nothing)
	if z := parseVentiStorage("index=x\n"); z.total != 0 {
		t.Errorf("no-data total = %d, want 0", z.total)
	}
}

func TestParseGraphRaw(t *testing.T) {
	// dotextbin output: a "stats" header then bins. Empty bins (nsamp=0) are ignored.
	// The current cumulative value is the largest max among sampled bins.
	data := "stats\n\n" +
		"0: nsamp=0 min=0 max=0 avg=0\n" +
		"1: nsamp=1 min=100 max=100 avg=100\n" +
		"2: nsamp=2 min=140 max=150 avg=145\n" +
		"3: nsamp=0 min=0 max=0 avg=0\n"
	v, ok := parseGraphRaw(data)
	if !ok || v != 150 {
		t.Errorf("parseGraphRaw = %d ok=%v, want 150 true", v, ok)
	}
	// venti prints these 32-bit counters through %d, so a value in [2^31,2^32)
	// arrives negative and must decode as the wrapped 32-bit value.
	neg := "stats\n\n" +
		"0: nsamp=1 min=2147483000 max=2147483000 avg=2147483000\n" +
		"1: nsamp=2 min=2147483000 max=-2147483000 avg=-2147483324\n"
	v, ok = parseGraphRaw(neg)
	if !ok || v != 2147484296 { // 2^32 - 2147483000
		t.Errorf("negative-printed max = %d ok=%v, want 2147484296 true", v, ok)
	}
	// all-empty (collectstats off / no history yet) -> ok=false
	if _, ok := parseGraphRaw("stats\n\n0: nsamp=0 min=0 max=0 avg=0\n"); ok {
		t.Error("all-empty bins should yield ok=false")
	}
	if _, ok := parseGraphRaw("garbage\n"); ok {
		t.Error("garbage should yield ok=false")
	}
}

// TestGraphSeriesRates drives the stateful /graph rate path against a fake venti:
// the first scrape only establishes the baseline, an advancing counter then ships
// (cur-prev)/dt, a counter that goes backwards (venti restart) is skipped for that
// interval, and the reset value becomes the next baseline.
func TestGraphSeriesRates(t *testing.T) {
	vals := map[string]uint64{} // stat -> cumulative value served by the fake /graph
	venti := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := vals[r.URL.Query().Get("arg")]
		if !ok {
			io.WriteString(w, "stats\n\n0: nsamp=0 min=0 max=0 avg=0\n") // no history
			return
		}
		fmt.Fprintf(w, "stats\n\n0: nsamp=1 min=%d max=%d avg=%d\n", v, v, v)
	}))
	defer venti.Close()

	r := New(Options{VentiURL: venti.URL, Hostname: "cetus"})
	t0 := time.Unix(1700000000, 0)
	steps := []struct {
		name string
		at   time.Duration      // scrape time, offset from t0
		vals map[string]uint64  // counter values the fake venti serves
		want map[string]float64 // exactly the series the scrape must ship
	}{
		{"baseline", 0,
			map[string]uint64{"rpcread": 1000, "icachehit": 500},
			map[string]float64{}},
		{"advance and reset", 10 * time.Second,
			map[string]uint64{"rpcread": 1150, "icachehit": 20}, // +150 over 10s, icachehit reset
			map[string]float64{"venti.rpc.reads": 15}},          // the reset counter is skipped
		{"resume after reset", 20 * time.Second,
			map[string]uint64{"rpcread": 1150, "icachehit": 80}, // +60 over 10s from the new baseline
			map[string]float64{"venti.rpc.reads": 0, "venti.icache.hits": 6}},
	}
	for _, st := range steps {
		for k, v := range st.vals {
			vals[k] = v
		}
		got := map[string]float64{}
		for _, s := range r.graphSeries(context.Background(), t0.Add(st.at)) {
			got[s.Name] = s.Points[0].Value
		}
		if len(got) != len(st.want) {
			t.Errorf("%s: series = %v, want %v", st.name, got, st.want)
			continue
		}
		for name, w := range st.want {
			if v, ok := got[name]; !ok || v != w {
				t.Errorf("%s: %s = %v (present=%v), want %v", st.name, name, v, ok, w)
			}
		}
	}
}

func TestStorageSeries(t *testing.T) {
	r := New(Options{Hostname: "cetus", Tags: []string{"env:prod"}, Interval: 15 * time.Second})
	st := ventiStorage{
		total: 2048, used: 512, // bytes -> kB: total 2, used 0.5, free 1.5, in_use 0.25
		arenas: 12, arenasActive: 8, clumps: 1000, cclumps: 900,
		uncBytes: 2000, compBytes: 1000, // compression_ratio = 2.0
	}
	series := r.storageSeries(st)
	want := map[string]float64{
		"system.disk.total":             2,
		"system.disk.used":              0.5,
		"system.disk.free":              1.5,
		"system.disk.in_use":            0.25,
		"venti.arenas.total":            12,
		"venti.arenas.active":           8,
		"venti.clumps.total":            1000,
		"venti.clumps.compressed":       900,
		"venti.data.uncompressed_bytes": 2000,
		"venti.data.compressed_bytes":   1000,
		"venti.data.compression_ratio":  2,
	}
	if len(series) != len(want) {
		t.Fatalf("len = %d, want %d", len(series), len(want))
	}
	for _, s := range series {
		if s.Host != "cetus" {
			t.Errorf("%s host = %q, want cetus", s.Name, s.Host)
		}
		w, ok := want[s.Name]
		if !ok {
			t.Errorf("unexpected metric %s", s.Name)
			continue
		}
		if s.Points[0].Value != w {
			t.Errorf("%s = %v, want %v", s.Name, s.Points[0].Value, w)
		}
		// system.disk.* carry device:venti. The venti.* metrics do not.
		hasDev := false
		for _, tag := range s.Tags {
			if tag == "device:venti" {
				hasDev = true
			}
		}
		if wantDev := strings.HasPrefix(s.Name, "system.disk."); hasDev != wantDev {
			t.Errorf("%s device:venti = %v, want %v", s.Name, hasDev, wantDev)
		}
	}
}

// TestSendEmitsVentiUp drives the full send() path against a fake venti and a fake
// intake: a reachable venti ships venti.up=1 plus the disk series, an unreachable one
// (HTTP 500) ships venti.up=0 and no disk series, so a down venti is a clear signal
// rather than a silent gap.
func TestSendEmitsVentiUp(t *testing.T) {
	const storage = "index=main\ntotal arenas=12 active=8\n" +
		"total space=2048 used=512\nclumps=10 compressed clumps=9 data=2000 compressed data=1000\n"
	for _, tc := range []struct {
		name     string
		ventiOK  bool // false => venti returns 500 (down)
		wantUp   float64
		wantDisk bool
	}{
		{name: "up", ventiOK: true, wantUp: 1, wantDisk: true},
		{name: "down", ventiOK: false, wantUp: 0, wantDisk: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			venti := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !tc.ventiOK {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if strings.HasPrefix(r.URL.Path, "/storage") {
					io.WriteString(w, storage)
				}
				// /graph: empty 200 body (no collectstats), yields no rate metrics.
			}))
			defer venti.Close()

			got := map[string]float64{}
			intakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gr, err := gzip.NewReader(r.Body)
				if err != nil {
					t.Errorf("gzip: %v", err)
					return
				}
				body, _ := io.ReadAll(gr)
				var payload struct {
					Series []struct {
						Metric string      `json:"metric"`
						Points [][]float64 `json:"points"`
					} `json:"series"`
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Errorf("unmarshal: %v", err)
				}
				for _, s := range payload.Series {
					if len(s.Points) > 0 && len(s.Points[0]) == 2 {
						got[s.Metric] = s.Points[0][1]
					}
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			defer intakeSrv.Close()

			r := New(Options{
				Client:           intake.New(intake.Options{}),
				MetricsEndpoints: []intake.Endpoint{{URL: intakeSrv.URL + "/api/v1/series", APIKey: "k"}},
				VentiURL:         venti.URL,
				Hostname:         "cetus",
			})
			r.send(context.Background())

			if up, ok := got["venti.up"]; !ok || up != tc.wantUp {
				t.Errorf("venti.up = %v (present=%v), want %v", up, ok, tc.wantUp)
			}
			if _, ok := got["system.disk.total"]; ok != tc.wantDisk {
				t.Errorf("system.disk.total present = %v, want %v", ok, tc.wantDisk)
			}
		})
	}
}
