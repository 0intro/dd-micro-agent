package metrics

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

func TestSendEventsEnvelope(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/intake/" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gr, _ := gzip.NewReader(r.Body)
		body, _ = io.ReadAll(gr)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := intake.New(intake.Options{})
	eps := []intake.Endpoint{{URL: srv.URL + "/intake/", APIKey: "KEY"}}
	events := []Event{
		{Title: "deploy", Text: "v1\nshipped", Timestamp: 100, AlertType: "success", Host: "h"},
		{Title: "alert", Text: "high", AlertType: "error", SourceTypeName: "custom"},
	}
	if err := SendEvents(context.Background(), c, eps, "host1", events); err != nil {
		t.Fatal(err)
	}
	var env struct {
		APIKey           string             `json:"apiKey"`
		Events           map[string][]Event `json:"events"`
		InternalHostname string             `json:"internalHostname"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("envelope: %v (%s)", err, body)
	}
	if env.APIKey != "KEY" || env.InternalHostname != "host1" {
		t.Errorf("apiKey/internalHostname = %q/%q", env.APIKey, env.InternalHostname)
	}
	// Grouped by source type: one under the default "api", one under "custom".
	if len(env.Events["api"]) != 1 || env.Events["api"][0].Title != "deploy" {
		t.Errorf("api group = %+v", env.Events["api"])
	}
	if len(env.Events["custom"]) != 1 || env.Events["custom"][0].Title != "alert" {
		t.Errorf("custom group = %+v", env.Events["custom"])
	}
}

func TestMarshalEventsFieldNames(t *testing.T) {
	// The intake field names must be msg_title/msg_text/timestamp, not title/text.
	b, err := MarshalEvents([]Event{{Title: "T", Text: "X", Timestamp: 5}}, "k", "h")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"msg_title":"T"`, `"msg_text":"X"`, `"timestamp":5`, `"apiKey":"k"`, `"internalHostname":"h"`} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s: %s", want, s)
		}
	}
}

func TestAggregatorFlushesEvents(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gr, _ := gzip.NewReader(r.Body)
		body, _ = io.ReadAll(gr)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	a := newAgg(AggregatorOptions{
		Hostname: "h1", Tags: []string{"env:prod"},
		Client:          intake.New(intake.Options{}),
		EventsEndpoints: []intake.Endpoint{{URL: srv.URL + "/intake/", APIKey: "k"}},
	})
	a.pendingEvents = []Event{{Title: "t", Text: "x"}} // no host/timestamp/tags
	a.flush(time.Unix(2000, 0))

	var env struct {
		Events map[string][]Event `json:"events"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body: %v (%s)", err, body)
	}
	ev := env.Events["api"]
	if len(ev) != 1 || ev[0].Host != "h1" || ev[0].Timestamp != 2000 {
		t.Errorf("host/timestamp not finalized: %s", body)
	}
	if len(ev[0].Tags) != 1 || ev[0].Tags[0] != "env:prod" {
		t.Errorf("global tags not appended: %s", body)
	}
	if len(a.pendingEvents) != 0 {
		t.Error("pendingEvents not cleared after flush")
	}
}
