package process

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

// fakeIntake records submissions and replies with a ResCollector that toggles
// realtime per the configured ActiveClients/Interval.
type fakeIntake struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	active   int32
	interval int32
}

func (f *fakeIntake) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()

	var status pbuf
	status.uint(1, uint64(uint32(f.active)))
	status.uint(2, uint64(uint32(f.interval)))
	var res pbuf
	res.msg(3, status.b)
	w.Write(frame(typeResCollector, res.b))
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestReporterSendProcess(t *testing.T) {
	fake := &fakeIntake{active: 1, interval: 2}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	r := New(Options{Endpoints: []intake.Endpoint{{URL: srv.URL, APIKey: "secret"}}, Hostname: "test-host", Logger: discardLogger()})
	r.send(context.Background(), time.Now(), false)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.bodies) == 0 {
		t.Fatal("intake received no submissions")
	}
	// Headers: protobuf content type and the API key.
	if ct := fake.requests[0].Header.Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("Content-Type = %q", ct)
	}
	if k := fake.requests[0].Header.Get("DD-Api-Key"); k != "secret" {
		t.Errorf("DD-Api-Key = %q", k)
	}
	if h := fake.requests[0].Header.Get("X-Dd-Hostname"); h != "test-host" {
		t.Errorf("X-Dd-Hostname = %q", h)
	}

	// Every chunk is a CollectorProc for our host. Together they hold processes.
	var host string
	var total int
	for _, b := range fake.bodies {
		body, typ, ok := unframe(b)
		if !ok || typ != typeCollectorProc {
			t.Fatalf("bad frame: typ=%d ok=%v", typ, ok)
		}
		h, procs := decodeCollectorProc(t, body)
		host = h
		total += len(procs)
	}
	if host != "test-host" {
		t.Errorf("hostname = %q, want test-host", host)
	}
	if total == 0 {
		t.Error("payload carried no processes")
	}
	if !r.rtEnabled {
		t.Error("realtime should be enabled after ActiveClients=1 response")
	}
	if r.rtInterval != 2*time.Second {
		t.Errorf("rtInterval = %v, want 2s", r.rtInterval)
	}
}

// Dual-shipping: every chunk fans out to the additional endpoint under its own
// key, the primary alone drives the realtime toggle, and a failing secondary
// changes nothing.
func TestReporterFanOutSecondaryBestEffort(t *testing.T) {
	prim := &fakeIntake{active: 1, interval: 2}
	primSrv := httptest.NewServer(http.HandlerFunc(prim.handler))
	defer primSrv.Close()

	var (
		mu   sync.Mutex
		keys []string
	)
	secSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("DD-Api-Key"))
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest) // permanent, so the failure is immediate
	}))
	defer secSrv.Close()

	r := New(Options{
		Endpoints: []intake.Endpoint{
			{URL: primSrv.URL, APIKey: "ka", Reliable: true},
			{URL: secSrv.URL, APIKey: "kb", Reliable: true},
		},
		Hostname: "h",
		Logger:   discardLogger(),
	})
	r.send(context.Background(), time.Now(), false)

	prim.mu.Lock()
	nPrim := len(prim.bodies)
	prim.mu.Unlock()
	mu.Lock()
	nSec, firstKey := len(keys), ""
	if nSec > 0 {
		firstKey = keys[0]
	}
	mu.Unlock()

	if nPrim == 0 || nSec != nPrim {
		t.Fatalf("posts: primary %d, secondary %d, want equal and nonzero (per-chunk fan-out)", nPrim, nSec)
	}
	if firstKey != "kb" {
		t.Errorf("secondary DD-Api-Key = %q, want kb (its own key)", firstKey)
	}
	if !r.rtEnabled {
		t.Error("a failing secondary must not affect the primary-driven realtime toggle")
	}
}

func TestReporterRealtimeStream(t *testing.T) {
	fake := &fakeIntake{active: 0} // clients gone -> realtime should disable
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	r := New(Options{Endpoints: []intake.Endpoint{{URL: srv.URL, APIKey: "k"}}, Hostname: "h", Logger: discardLogger()})
	r.rtEnabled = true
	r.send(context.Background(), time.Now(), true) // realtime send

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.bodies) == 0 {
		t.Fatal("no realtime submission")
	}
	_, typ, ok := unframe(fake.bodies[0])
	if !ok || typ != typeCollectorRealTime {
		t.Fatalf("realtime frame type = %d, want %d", typ, typeCollectorRealTime)
	}
	if r.rtEnabled {
		t.Error("realtime should disable after ActiveClients=0 response")
	}
}

func TestChunk(t *testing.T) {
	mk := func(n int) []*Proc {
		ps := make([]*Proc, n)
		for i := range ps {
			ps[i] = &Proc{Pid: int32(i + 1)}
		}
		return ps
	}
	if got := chunk(nil, 100); got != nil {
		t.Errorf("chunk(nil) = %v, want nil", got)
	}
	if got := chunk(mk(50), 100); len(got) != 1 || len(got[0]) != 50 {
		t.Errorf("chunk(50,100) = %d chunks", len(got))
	}
	got := chunk(mk(250), 100)
	if len(got) != 3 || len(got[0]) != 100 || len(got[2]) != 50 {
		t.Errorf("chunk(250,100) sizes = %d,%d,...,%d", len(got), len(got[0]), len(got[len(got)-1]))
	}
}

func TestCreateTimePinnedAgainstJitter(t *testing.T) {
	// A Plan 9-style create time derived as now-age wobbles by a tick each collect.
	// rate() must pin it to the first value seen so the (pid, createTime) identity
	// stays stable across snapshots (and the CPU diff keeps working).
	r := New(Options{Hostname: "h", Logger: discardLogger()})
	t0 := time.Now()
	first := r.rate([]Proc{{Pid: 7, CreateTime: 1_700_000_000_000, UserTime: 1}}, t0)
	pinned := first[0].CreateTime

	// Next collects report a create time jittering by a few ms: it must not move,
	// and CPU% must compute (proving the diff still matches the same process).
	out := r.rate([]Proc{{Pid: 7, CreateTime: 1_700_000_000_013, UserTime: 2}}, t0.Add(time.Second))
	if out[0].CreateTime != pinned {
		t.Errorf("create time moved under jitter: got %d, want pinned %d", out[0].CreateTime, pinned)
	}
	if out[0].TotalPct == 0 {
		t.Error("CPU%% should compute across the pinned identity")
	}
	// A genuine PID reuse (create time jumps far) is NOT pinned: identity resets.
	reuse := r.rate([]Proc{{Pid: 7, CreateTime: 1_700_000_500_000, UserTime: 99}}, t0.Add(2*time.Second))
	if reuse[0].CreateTime == pinned {
		t.Error("PID reuse should reset the create time, not pin to the old one")
	}
}

func TestRateNeedsTwoSamples(t *testing.T) {
	r := New(Options{Hostname: "h", Logger: discardLogger()})
	t0 := time.Now()
	procs := []Proc{{Pid: 1, CreateTime: 100, UserTime: 10, SystemTime: 5}}
	// First sample: no previous, so no percentages.
	out := r.rate(procs, t0)
	if out[0].TotalPct != 0 {
		t.Errorf("first sample TotalPct = %v, want 0", out[0].TotalPct)
	}
	// Second sample one second later: +1s user, +0.5s system over 1s wall.
	procs[0].UserTime, procs[0].SystemTime = 11, 5.5
	out = r.rate(procs, t0.Add(time.Second))
	if got := out[0].UserPct; got < 99 || got > 101 {
		t.Errorf("UserPct = %v, want ~100", got)
	}
	if got := out[0].SystemPct; got < 49 || got > 51 {
		t.Errorf("SystemPct = %v, want ~50", got)
	}
}
