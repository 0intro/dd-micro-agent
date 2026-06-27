package logs

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/config"
	"github.com/0intro/dd-micro-agent/internal/intake"
)

// collectingServer is a fake /api/v2/logs intake that gunzips each batch and
// appends every message to received.
func collectingServer(t *testing.T, mu *sync.Mutex, received *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/logs" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("not gzip: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(gr)
		var arr []map[string]any
		if err := json.Unmarshal(body, &arr); err != nil {
			t.Errorf("bad json: %v", err)
		}
		mu.Lock()
		for _, m := range arr {
			*received = append(*received, m["message"].(string))
		}
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// waitFor polls cond until it holds or a deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The agent tails from the end (no history backfill): pre-existing content is not
// shipped, lines appended after startup are, and a fresh agent resumes from the
// registry without re-sending.
func TestAgentDeliversAndResumes(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
	)
	srv := collectingServer(t, &mu, &received)

	runPath := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logPath, []byte("old1\nold2\n"), 0o644); err != nil { // history, must be skipped
		t.Fatal(err)
	}

	buf1, log1 := captureLogger()
	opts := Options{
		Sources:      []config.LogSource{{Type: "file", Path: logPath, Service: "svc"}},
		Client:       intake.New(intake.Options{}),
		Endpoints:    []intake.Endpoint{{URL: srv.URL + "/api/v2/logs", APIKey: "k"}},
		Hostname:     "h",
		RunPath:      runPath,
		BatchWait:    20 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		Logger:       log1,
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(received) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { NewAgent(opts).Run(ctx); close(done) }()
	waitFor(t, tailing(buf1, logPath)) // the tailer is at EOF, appends are new
	appendFile(t, logPath, "a\nb\nc\n")
	waitFor(t, func() bool { return count() >= 3 })
	cancel()
	<-done

	mu.Lock()
	got := append([]string(nil), received...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("received %v, want [a b c] (pre-existing history must be skipped)", got)
	}

	// Offset should be at the end of the whole file: 10 bytes history + 6 appended.
	reg := LoadRegistry(filepath.Join(runPath, "registry.json"), nil)
	if off := reg.Offset("file:" + logPath); off != 16 {
		t.Errorf("registry offset = %d, want 16", off)
	}

	// A fresh agent over the same run path must not re-send, and ships new appends.
	mu.Lock()
	received = nil
	mu.Unlock()
	buf2, log2 := captureLogger()
	opts.Logger = log2
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { NewAgent(opts).Run(ctx2); close(done2) }()
	waitFor(t, tailing(buf2, logPath))
	appendFile(t, logPath, "d\n")
	waitFor(t, func() bool { return count() >= 1 })
	cancel2()
	<-done2

	mu.Lock()
	got2 := append([]string(nil), received...)
	mu.Unlock()
	if !reflect.DeepEqual(got2, []string{"d"}) {
		t.Errorf("resume received %v, want [d] (a/b/c must not be re-sent)", got2)
	}
}

// Dual-shipping: a down additional endpoint must not block delivery to the primary
// nor stall the registry offset. The primary alone gates the at-least-once contract.
func TestAgentDualShipAdditionalFailureDoesNotStallOffset(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
	)
	primary := collectingServer(t, &mu, &received)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer down.Close()

	runPath := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	opts := Options{
		Sources: []config.LogSource{{Type: "file", Path: logPath, Service: "svc"}},
		Client:  intake.New(intake.Options{Backoff: time.Millisecond, MaxTries: 2}),
		Endpoints: []intake.Endpoint{
			{URL: primary.URL + "/api/v2/logs", APIKey: "ka", Reliable: true},
			{URL: down.URL + "/api/v2/logs", APIKey: "kb", Reliable: true},
		},
		Hostname:     "h",
		RunPath:      runPath,
		BatchWait:    20 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		Logger:       discardLogger(),
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(received) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { NewAgent(opts).Run(ctx); close(done) }()
	time.Sleep(150 * time.Millisecond)
	appendFile(t, logPath, "x\ny\n")
	waitFor(t, func() bool { return count() >= 2 })
	cancel()
	<-done

	reg := LoadRegistry(filepath.Join(runPath, "registry.json"), nil)
	if off := reg.Offset("file:" + logPath); off != 4 { // "x\ny\n"
		t.Errorf("registry offset = %d, want 4 (a failed additional endpoint must not stall the offset)", off)
	}
}

// A transient intake failure must not lose lines: the batcher keeps the failed
// batch and retries it, so everything appended is eventually delivered in order
// and the registry ends at the true high-water mark.
func TestAgentRetriesFailedBatch(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
		fails    = 2
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("not gzip: %v", err)
			return
		}
		body, _ := io.ReadAll(gr)
		var arr []map[string]any
		if err := json.Unmarshal(body, &arr); err != nil {
			t.Errorf("bad json: %v", err)
		}
		for _, m := range arr {
			received = append(received, m["message"].(string))
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	runPath := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Sources:      []config.LogSource{{Type: "file", Path: logPath, Service: "svc"}},
		Client:       intake.New(intake.Options{Backoff: time.Millisecond, MaxTries: 1}),
		Endpoints:    []intake.Endpoint{{URL: srv.URL + "/api/v2/logs", APIKey: "k"}},
		Hostname:     "h",
		RunPath:      runPath,
		BatchWait:    20 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		Logger:       discardLogger(),
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(received) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { NewAgent(opts).Run(ctx); close(done) }()
	time.Sleep(150 * time.Millisecond)
	appendFile(t, logPath, "a\nb\n")
	waitFor(t, func() bool { return count() >= 2 })
	cancel()
	<-done

	mu.Lock()
	got := append([]string(nil), received...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("received %v, want [a b] after retries", got)
	}
	reg := LoadRegistry(filepath.Join(runPath, "registry.json"), nil)
	if off := reg.Offset("file:" + logPath); off != 4 {
		t.Errorf("registry offset = %d, want 4", off)
	}
}

// A payload the intake refuses outright (a permanent status) is dropped with a
// warning rather than retried forever, and later batches keep flowing.
func TestAgentDropsRefusedBatch(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusBadRequest) // permanent: refused outright
			return
		}
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("not gzip: %v", err)
			return
		}
		body, _ := io.ReadAll(gr)
		var arr []map[string]any
		if err := json.Unmarshal(body, &arr); err != nil {
			t.Errorf("bad json: %v", err)
		}
		for _, m := range arr {
			received = append(received, m["message"].(string))
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	logPath := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Sources:      []config.LogSource{{Type: "file", Path: logPath, Service: "svc"}},
		Client:       intake.New(intake.Options{Backoff: time.Millisecond, MaxTries: 1}),
		Endpoints:    []intake.Endpoint{{URL: srv.URL + "/api/v2/logs", APIKey: "k"}},
		Hostname:     "h",
		RunPath:      t.TempDir(),
		BatchWait:    20 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		Logger:       discardLogger(),
	}
	seen := func() int { mu.Lock(); defer mu.Unlock(); return requests }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { NewAgent(opts).Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	time.Sleep(150 * time.Millisecond)
	appendFile(t, logPath, "refused\n")
	waitFor(t, func() bool { return seen() >= 1 }) // the 400 settles the first batch
	appendFile(t, logPath, "kept\n")
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(received) >= 1 })

	mu.Lock()
	got := append([]string(nil), received...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"kept"}) {
		t.Errorf("received %v, want [kept] (the refused batch dropped, later ones flowing)", got)
	}
}

func TestAgentGlobSource(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
	)
	srv := collectingServer(t, &mu, &received)

	dir := t.TempDir()
	ap, bp, cp := filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log"), filepath.Join(dir, "c.log")
	// a.log/b.log exist (empty) at startup so tail-from-end captures the appends.
	for _, p := range []string{ap, bp} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	buf, logger := captureLogger()
	opts := Options{
		Sources:      []config.LogSource{{Type: "file", Path: filepath.Join(dir, "*.log"), Service: "svc"}},
		Client:       intake.New(intake.Options{}),
		Endpoints:    []intake.Endpoint{{URL: srv.URL + "/api/v2/logs", APIKey: "k"}},
		Hostname:     "h",
		RunPath:      t.TempDir(),
		BatchWait:    20 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		ScanInterval: 30 * time.Millisecond, // fast rescan so the test isn't slow
		Logger:       logger,
	}
	has := func(s string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range received {
			if m == s {
				return true
			}
		}
		return false
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { NewAgent(opts).Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, tailing(buf, ap)) // discovery found a.log/b.log and both are at EOF
	waitFor(t, tailing(buf, bp))
	appendFile(t, ap, "a1\n")
	appendFile(t, bp, "b1\n")

	// c.log appears after startup: the rescan must discover it, then tail its appends.
	if err := os.WriteFile(cp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tailing(buf, cp))
	appendFile(t, cp, "c1\n")

	waitFor(t, func() bool { return has("a1") && has("b1") && has("c1") })
}

// A glob-discovered file that leaves the match set is done after two scans: its
// tailer stops, its registry entry goes, and a file reappearing at the same path
// is discovered fresh.
func TestAgentGlobPrunesDepartedFiles(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
	)
	srv := collectingServer(t, &mu, &received)

	dir := t.TempDir()
	bp := filepath.Join(dir, "b.log")
	if err := os.WriteFile(bp, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	runPath := t.TempDir()
	buf, logger := captureLogger()
	opts := Options{
		Sources:      []config.LogSource{{Type: "file", Path: filepath.Join(dir, "*.log"), Service: "svc"}},
		Client:       intake.New(intake.Options{}),
		Endpoints:    []intake.Endpoint{{URL: srv.URL + "/api/v2/logs", APIKey: "k"}},
		Hostname:     "h",
		RunPath:      runPath,
		BatchWait:    20 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		ScanInterval: 25 * time.Millisecond,
		Logger:       logger,
	}
	has := func(s string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range received {
			if m == s {
				return true
			}
		}
		return false
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { NewAgent(opts).Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, tailing(buf, bp))
	appendFile(t, bp, "b1\n")
	waitFor(t, func() bool { return has("b1") }) // an acknowledged offset exists

	if err := os.Remove(bp); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return buf.contains("stopped tailing") })
	// The periodic registry flush (1s) persists the pruned state.
	waitFor(t, func() bool {
		_, ok := LoadRegistry(filepath.Join(runPath, "registry.json"), nil).Position("file:" + bp)
		return !ok
	})

	// The path reappears: discovery starts a fresh tailer (tail from end).
	if err := os.WriteFile(bp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return buf.count(`msg="tailing from" path=`+bp) >= 2 })
	appendFile(t, bp, "b2\n")
	waitFor(t, func() bool { return has("b2") })
}
