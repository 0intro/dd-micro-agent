package metrics

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

func TestSendServiceChecks(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/check_run" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gr, _ := gzip.NewReader(r.Body)
		body, _ = io.ReadAll(gr)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := intake.New(intake.Options{})
	eps := []intake.Endpoint{{URL: srv.URL + "/api/v1/check_run", APIKey: "k"}}
	checks := []ServiceCheck{{Check: "app.up", Host: "h", Timestamp: 100, Status: ServiceCritical, Message: "down"}}
	if err := SendServiceChecks(context.Background(), c, eps, checks); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body not a JSON array: %v (%s)", err, body)
	}
	if len(got) != 1 || got[0]["check"] != "app.up" || got[0]["host_name"] != "h" || got[0]["status"].(float64) != 2 {
		t.Errorf("check_run body = %s", body)
	}
	if _, ok := got[0]["tags"].([]any); !ok {
		t.Errorf("tags should be an array (not null), got %v", got[0]["tags"])
	}
}

func TestSendServiceChecksEmptyIsNoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("should not POST for empty checks")
	}))
	defer srv.Close()
	c := intake.New(intake.Options{})
	eps := []intake.Endpoint{{URL: srv.URL + "/api/v1/check_run", APIKey: "k"}}
	if err := SendServiceChecks(context.Background(), c, eps, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAggregatorFlushesServiceChecks(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gr, _ := gzip.NewReader(r.Body)
		body, _ = io.ReadAll(gr)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	a := newAgg(AggregatorOptions{
		Hostname: "h1", Tags: []string{"env:prod"},
		Client:            intake.New(intake.Options{}),
		CheckRunEndpoints: []intake.Endpoint{{URL: srv.URL + "/api/v1/check_run", APIKey: "k"}},
	})
	a.pendingChecks = []ServiceCheck{{Check: "app.up", Status: ServiceOK}} // no host/ts/tags
	a.flush(time.Unix(1000, 0))

	var got []map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body: %v (%s)", err, body)
	}
	if got[0]["host_name"] != "h1" || got[0]["timestamp"].(float64) != 1000 {
		t.Errorf("host/timestamp not finalized: %s", body)
	}
	if tags, _ := got[0]["tags"].([]any); len(tags) != 1 || tags[0] != "env:prod" {
		t.Errorf("global tags not appended: %s", body)
	}
	if len(a.pendingChecks) != 0 {
		t.Error("pendingChecks not cleared after flush")
	}
}
