package logs

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

func TestMarshalLogsGolden(t *testing.T) {
	msgs := []Message{{
		Content:   []byte("hello world"),
		Timestamp: time.UnixMilli(1718539200123),
		Service:   "web",
		Source:    "nginx",
		Tags:      []string{"env:prod", "team:x"},
	}}
	got, err := MarshalLogs(msgs, "h1")
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"message":"hello world","status":"info","timestamp":1718539200123,"hostname":"h1","service":"web","ddsource":"nginx","ddtags":"env:prod,team:x"}]`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestMarshalLogsKeepsExplicitStatus(t *testing.T) {
	msgs := []Message{{
		Content:   []byte("oops"),
		Status:    "error",
		Timestamp: time.UnixMilli(1000),
	}}
	got, err := MarshalLogs(msgs, "h")
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"message":"oops","status":"error","timestamp":1000,"hostname":"h","service":"","ddsource":"","ddtags":""}]`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestSendLogsEndToEnd(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/logs" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gr, _ := gzip.NewReader(r.Body)
		body, _ = io.ReadAll(gr)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := intake.New(intake.Options{})
	eps := []intake.Endpoint{{URL: srv.URL + "/api/v2/logs", APIKey: "k"}}
	msgs := []Message{{Content: []byte("line"), Timestamp: time.UnixMilli(5), Service: "svc"}}
	if _, err := SendLogs(context.Background(), c, eps, "h1", msgs); err != nil {
		t.Fatal(err)
	}
	want := `[{"message":"line","status":"info","timestamp":5,"hostname":"h1","service":"svc","ddsource":"","ddtags":""}]`
	if string(body) != want {
		t.Errorf("server received %s\nwant %s", body, want)
	}
}
