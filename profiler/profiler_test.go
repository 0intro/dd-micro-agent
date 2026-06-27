package profiler

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestUpload runs one real collection cycle and uploads it, then checks the
// multipart the proxy receives. By default it uploads to an in-process httptest
// proxy and inspects the body. With PROFILE_PROXY_URL set it uploads to a real agent
// proxy instead (the Plan 9 VM leg) and only checks the upload was accepted, since
// the forwarded body is then asserted on the intake side.
func TestUpload(t *testing.T) {
	var gotCT string
	var gotBody []byte
	url := os.Getenv("PROFILE_PROXY_URL")
	live := url != ""
	if !live {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotCT = r.Header.Get("Content-Type")
			gotBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted) // the proxy replies 202, our client takes any 2xx
		}))
		defer srv.Close()
		url = srv.URL
	}

	p := &profiler{
		cfg: &config{
			service: "test", env: "ci", version: "1.2.3",
			url:    url,
			period: 50 * time.Millisecond,
			types:  []Type{HeapProfile, GoroutineProfile},
			log:    discardLogger(),
		},
		http:      &http.Client{Timeout: 10 * time.Second},
		hostname:  "host1",
		runtimeID: "rid",
	}

	bat, err := p.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := len(bat.profiles); got != 2 {
		t.Fatalf("collected %d profiles, want 2 (heap + goroutine)", got)
	}
	if err := p.upload(context.Background(), bat); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if live {
		return // the real proxy forwards the body, asserted intake-side by the VM e2e
	}

	_, params, err := mime.ParseMediaType(gotCT)
	if err != nil || params["boundary"] == "" {
		t.Fatalf("content-type %q not multipart: %v", gotCT, err)
	}
	var ev struct {
		Family      string   `json:"family"`
		Version     string   `json:"version"`
		Attachments []string `json:"attachments"`
		Tags        string   `json:"tags_profiler"`
	}
	var heap []byte
	mr := multipart.NewReader(bytes.NewReader(gotBody), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		switch {
		case part.FormName() == "event" || part.FileName() == "event.json":
			json.NewDecoder(part).Decode(&ev)
		case part.FileName() == "heap.pprof":
			heap, _ = io.ReadAll(part)
		}
		part.Close()
	}

	if ev.Family != "go" || ev.Version != "4" {
		t.Errorf("event family/version = %q/%q, want go/4", ev.Family, ev.Version)
	}
	for _, want := range []string{"heap.pprof", "goroutines.pprof"} {
		if !slices.Contains(ev.Attachments, want) {
			t.Errorf("attachments %v missing %q", ev.Attachments, want)
		}
	}
	for _, want := range []string{"service:test", "runtime:go", "env:ci", "version:1.2.3"} {
		if !strings.Contains(ev.Tags, want) {
			t.Errorf("tags_profiler %q missing %q", ev.Tags, want)
		}
	}
	if len(heap) == 0 {
		t.Fatal("heap.pprof attachment is empty")
	}
	// WriteTo(_, 0) emits gzip-compressed protobuf, so the attachment must gunzip.
	if _, err := gzip.NewReader(bytes.NewReader(heap)); err != nil {
		t.Errorf("heap.pprof is not gzip: %v", err)
	}
}

// TestStartStop exercises the package-level lifecycle: Start collects and uploads on
// its own goroutine, Stop halts it promptly.
func TestStartStop(t *testing.T) {
	uploaded := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select {
		case uploaded <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := Start(
		WithURL(srv.URL),
		WithService("test"),
		WithPeriod(30*time.Millisecond),
		WithProfileTypes(HeapProfile, GoroutineProfile),
		WithLogger(discardLogger()),
	); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-uploaded:
	case <-time.After(5 * time.Second):
		Stop()
		t.Fatal("no profile uploaded within 5s")
	}
	done := make(chan struct{})
	go func() { Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return promptly")
	}
}

// TestMutexBlockDefaults proves that requesting the mutex and block profiles
// implies dd-trace-go's default runtime sampling rates, since the runtime
// samples neither until its rate is set, and that an explicit option wins.
func TestMutexBlockDefaults(t *testing.T) {
	c := newConfig([]Option{WithProfileTypes(MutexProfile, BlockProfile)})
	if c.mutexFraction != defaultMutexFraction {
		t.Errorf("mutexFraction = %d, want default %d", c.mutexFraction, defaultMutexFraction)
	}
	if c.blockRate != defaultBlockRate {
		t.Errorf("blockRate = %d, want default %d", c.blockRate, defaultBlockRate)
	}

	c = newConfig([]Option{WithMutexFraction(2), WithBlockRate(5), WithProfileTypes(MutexProfile, BlockProfile)})
	if c.mutexFraction != 2 || c.blockRate != 5 {
		t.Errorf("explicit rates = %d/%d, want 2/5", c.mutexFraction, c.blockRate)
	}

	c = newConfig(nil) // without the types the rates stay unset
	if c.mutexFraction != 0 || c.blockRate != 0 {
		t.Errorf("rates without the types = %d/%d, want 0/0", c.mutexFraction, c.blockRate)
	}
}

func TestDefaultService(t *testing.T) {
	t.Setenv("DD_SERVICE", "")
	if c, want := newConfig(nil), filepath.Base(os.Args[0]); c.service != want {
		t.Errorf("service = %q, want executable name %q", c.service, want)
	}
	t.Setenv("DD_SERVICE", "svc-env")
	if c := newConfig(nil); c.service != "svc-env" {
		t.Errorf("service = %q, want svc-env", c.service)
	}
	if c := newConfig([]Option{WithService("svc-opt")}); c.service != "svc-opt" {
		t.Errorf("service = %q, want svc-opt", c.service)
	}
}

// TestCollectExecutionTrace runs one cycle with the execution trace on, which
// exercises the bounded trace writer end to end.
func TestCollectExecutionTrace(t *testing.T) {
	p := &profiler{cfg: &config{
		period: 50 * time.Millisecond,
		types:  []Type{ExecutionTrace},
		log:    discardLogger(),
	}}
	bat, err := p.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(bat.profiles) != 1 || bat.profiles[0].name != "go.trace" {
		t.Fatalf("collected %d profiles, want one go.trace", len(bat.profiles))
	}
	if len(bat.profiles[0].data) == 0 {
		t.Fatal("go.trace attachment is empty")
	}
}

// TestTraceWriterBound checks the execution trace budget: full closes exactly
// once when the buffered bytes reach the limit, and later writes still land.
func TestTraceWriterBound(t *testing.T) {
	w := &traceWriter{limit: 8, full: make(chan struct{})}
	w.Write(make([]byte, 4))
	select {
	case <-w.full:
		t.Fatal("full closed below the limit")
	default:
	}
	w.Write(make([]byte, 4))
	select {
	case <-w.full:
	default:
		t.Fatal("full not closed at the limit")
	}
	w.Write(make([]byte, 4)) // a write past the limit must not close full again
	if w.buf.Len() != 12 {
		t.Errorf("buffered %d bytes, want 12", w.buf.Len())
	}
}

func TestDefaultProfileTypes(t *testing.T) {
	if got := defaultProfileTypes("plan9"); slices.Contains(got, CPUProfile) {
		t.Errorf("plan9 default %v includes CPUProfile, want it dropped", got)
	}
	if got := defaultProfileTypes("linux"); !slices.Contains(got, CPUProfile) {
		t.Errorf("linux default %v missing CPUProfile", got)
	}
	for _, goos := range []string{"plan9", "linux"} {
		got := defaultProfileTypes(goos)
		if !slices.Contains(got, HeapProfile) || !slices.Contains(got, GoroutineProfile) {
			t.Errorf("%s default %v missing heap/goroutine", goos, got)
		}
	}
}
