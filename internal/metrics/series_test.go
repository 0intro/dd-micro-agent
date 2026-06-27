package metrics

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

func TestMarshalSeriesGolden(t *testing.T) {
	series := []Serie{{
		Name:     "custom.metric",
		Points:   []Point{{Ts: 1718539200, Value: 12.5}},
		Type:     TypeGauge,
		Interval: 15,
		Tags:     []string{"env:prod"},
		Host:     "h1",
	}}
	got, err := MarshalSeries(series)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"series":[{"metric":"custom.metric","points":[[1718539200,12.5]],"type":"gauge","interval":15,"tags":["env:prod"],"host":"h1"}]}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestMarshalSeriesLiftsDeviceTag(t *testing.T) {
	series := []Serie{{
		Name:     "system.disk.total",
		Points:   []Point{{Ts: 1718539200, Value: 1024}},
		Type:     TypeGauge,
		Interval: 15,
		Tags:     []string{"device:/dev/sda1", "env:prod"},
		Host:     "h1",
	}}
	got, err := MarshalSeries(series)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"series":[{"metric":"system.disk.total","points":[[1718539200,1024]],"type":"gauge","interval":15,"tags":["env:prod"],"host":"h1","device":"/dev/sda1"}]}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestMarshalSeriesEmptyTagsRenderAsArray(t *testing.T) {
	got, err := MarshalSeries([]Serie{{Name: "m", Type: TypeGauge, Interval: 15, Host: "h"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"series":[{"metric":"m","points":null,"type":"gauge","interval":15,"tags":[],"host":"h"}]}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// A serie with a non-finite point is skipped rather than failing the whole
// payload: encoding/json refuses NaN and Inf.
func TestMarshalSeriesSkipsNonFinitePoints(t *testing.T) {
	body, err := MarshalSeries([]Serie{
		{Name: "bad", Type: TypeGauge, Points: []Point{{Ts: 1, Value: math.NaN()}}},
		{Name: "good", Type: TypeGauge, Points: []Point{{Ts: 1, Value: 2}}},
		{Name: "worse", Type: TypeGauge, Points: []Point{{Ts: 1, Value: math.Inf(1)}}},
	})
	if err != nil {
		t.Fatalf("MarshalSeries: %v", err)
	}
	var payload struct {
		Series []struct {
			Metric string `json:"metric"`
		} `json:"series"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if len(payload.Series) != 1 || payload.Series[0].Metric != "good" {
		t.Errorf("series = %+v, want only the finite one", payload.Series)
	}
}

func TestMarshalSeriesSourceTypeName(t *testing.T) {
	body, err := MarshalSeries([]Serie{{Name: "m", Type: TypeGauge, Points: []Point{{Ts: 1, Value: 2}}, SourceTypeName: "System"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `"source_type_name":"System"`; !strings.Contains(string(body), want) {
		t.Errorf("payload %s does not carry %s", body, want)
	}
}

func TestSendSeriesEndToEnd(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/series" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gr, _ := gzip.NewReader(r.Body)
		body, _ = io.ReadAll(gr)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := intake.New(intake.Options{})
	eps := []intake.Endpoint{{URL: srv.URL + "/api/v1/series", APIKey: "k"}}
	series := []Serie{{Name: "m", Points: []Point{{Ts: 1, Value: 2}}, Type: TypeGauge, Interval: 15, Host: "h"}}
	if err := SendSeries(context.Background(), c, eps, series); err != nil {
		t.Fatal(err)
	}
	want := `{"series":[{"metric":"m","points":[[1,2]],"type":"gauge","interval":15,"tags":[],"host":"h"}]}`
	if string(body) != want {
		t.Errorf("server received %s\nwant %s", body, want)
	}
}

func TestSendSeriesEmptyIsNoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("should not POST for empty series")
	}))
	defer srv.Close()
	c := intake.New(intake.Options{})
	eps := []intake.Endpoint{{URL: srv.URL + "/api/v1/series", APIKey: "k"}}
	if err := SendSeries(context.Background(), c, eps, nil); err != nil {
		t.Fatal(err)
	}
}
