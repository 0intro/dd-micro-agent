// Package profiler collects Go runtime profiles and uploads them to the Datadog
// profiling intake through a Datadog agent's /profiling/v1/input proxy. It is a
// pure-stdlib alternative to dd-trace-go's profiler for places that one does not
// reach, most notably Plan 9: the profile types that need no OS support (heap,
// goroutine, mutex, block, and the execution trace) work everywhere, while CPU
// profiling is dropped on Plan 9, where the Go runtime has no profiling interrupts
// and the profile is always empty.
//
// A Go program imports this package and calls Start once, like dd-trace-go:
//
//	profiler.Start(profiler.WithService("my-service"))
//	defer profiler.Stop()
package profiler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// uploadPath is the agent endpoint dd-trace-go and ddprof upload to.
const uploadPath = "/profiling/v1/input"

// Type is a kind of profile to collect. CPUProfile and ExecutionTrace span a
// whole period. The rest are instantaneous snapshots.
type Type int

// The profile types. CPUProfile records where CPU time is spent, HeapProfile
// live allocations, GoroutineProfile the stacks of all goroutines, MutexProfile
// contended lock holders, BlockProfile where goroutines block, and
// ExecutionTrace the runtime execution trace, capped at 5 MB per period.
const (
	CPUProfile Type = iota
	HeapProfile
	GoroutineProfile
	MutexProfile
	BlockProfile
	ExecutionTrace
)

// filename is the attachment name Datadog expects for the profile.
func (t Type) filename() string {
	switch t {
	case CPUProfile:
		return "cpu.pprof"
	case HeapProfile:
		return "heap.pprof"
	case GoroutineProfile:
		return "goroutines.pprof"
	case MutexProfile:
		return "mutex.pprof"
	case BlockProfile:
		return "block.pprof"
	case ExecutionTrace:
		return "go.trace"
	}
	return ""
}

// lookup is the runtime/pprof profile name for the snapshot types.
func (t Type) lookup() string {
	switch t {
	case HeapProfile:
		return "heap"
	case GoroutineProfile:
		return "goroutine"
	case MutexProfile:
		return "mutex"
	case BlockProfile:
		return "block"
	}
	return ""
}

// defaultProfileTypes returns the profiles collected by default for goos. CPU is
// dropped on Plan 9, where the runtime's setThreadCPUProfiler is a stub (it has no
// profiling interrupts), so the CPU profile is always empty there.
func defaultProfileTypes(goos string) []Type {
	if goos == "plan9" {
		return []Type{HeapProfile, GoroutineProfile}
	}
	return []Type{CPUProfile, HeapProfile, GoroutineProfile}
}

// dd-trace-go's default sampling rates, applied when MutexProfile or
// BlockProfile is requested without an explicit rate option. The runtime
// samples neither profile until its rate is set, so a requested type with a
// zero rate would upload permanently empty profiles.
const (
	defaultMutexFraction = 10
	defaultBlockRate     = 100000000
)

// Option configures the profiler.
type Option func(*config)

type config struct {
	service, env, version string
	tags                  []string
	url                   string // the agent's full upload URL
	period                time.Duration
	types                 []Type
	mutexFraction         int
	blockRate             int
	log                   *slog.Logger
}

// WithService sets the service tag (default DD_SERVICE, else the executable's
// base name).
func WithService(s string) Option { return func(c *config) { c.service = s } }

// WithEnv sets the env tag (default DD_ENV).
func WithEnv(s string) Option { return func(c *config) { c.env = s } }

// WithVersion sets the version tag (default DD_VERSION).
func WithVersion(s string) Option { return func(c *config) { c.version = s } }

// WithTags adds extra "key:value" tags (default DD_TAGS).
func WithTags(tags ...string) Option { return func(c *config) { c.tags = append(c.tags, tags...) } }

// WithAgentAddr sets the agent host:port the profiler uploads through (default from
// DD_AGENT_HOST, else 127.0.0.1:8126).
func WithAgentAddr(hostport string) Option {
	return func(c *config) { c.url = "http://" + hostport + uploadPath }
}

// WithURL sets the full upload URL, overriding WithAgentAddr.
func WithURL(url string) Option { return func(c *config) { c.url = url } }

// WithPeriod sets how often a profile is collected and uploaded (default 60s). The
// CPU profile and execution trace span one period.
func WithPeriod(d time.Duration) Option { return func(c *config) { c.period = d } }

// WithProfileTypes sets the exact profile types to collect, overriding the default.
func WithProfileTypes(types ...Type) Option { return func(c *config) { c.types = types } }

// WithMutexFraction sets the sampling fraction runtime.SetMutexProfileFraction
// is given when MutexProfile is collected (default 10). It does not enable the
// profile itself, add MutexProfile via WithProfileTypes for that.
func WithMutexFraction(n int) Option { return func(c *config) { c.mutexFraction = n } }

// WithBlockRate sets the sampling rate runtime.SetBlockProfileRate is given
// when BlockProfile is collected (default 100000000, one sample per 100ms
// blocked). It does not enable the profile itself, add BlockProfile via
// WithProfileTypes for that.
func WithBlockRate(n int) Option { return func(c *config) { c.blockRate = n } }

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.log = l } }

func newConfig(opts []Option) *config {
	service := os.Getenv("DD_SERVICE")
	if service == "" {
		service = filepath.Base(os.Args[0]) // dd-trace-go's default
	}
	c := &config{
		service: service,
		env:     os.Getenv("DD_ENV"),
		version: os.Getenv("DD_VERSION"),
		tags:    splitTags(os.Getenv("DD_TAGS")),
		url:     agentURL(),
		period:  60 * time.Second,
		types:   defaultProfileTypes(runtime.GOOS),
		log:     slog.Default(),
	}
	for _, o := range opts {
		o(c)
	}
	if c.mutexFraction == 0 && slices.Contains(c.types, MutexProfile) {
		c.mutexFraction = defaultMutexFraction
	}
	if c.blockRate == 0 && slices.Contains(c.types, BlockProfile) {
		c.blockRate = defaultBlockRate
	}
	return c
}

// agentURL is the default upload URL from the environment, the same variables
// dd-trace-go reads: DD_TRACE_AGENT_URL wins, else DD_AGENT_HOST on port 8126.
func agentURL() string {
	if u := os.Getenv("DD_TRACE_AGENT_URL"); u != "" {
		return strings.TrimRight(u, "/") + uploadPath
	}
	host := os.Getenv("DD_AGENT_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + host + ":8126" + uploadPath
}

func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
}

// profiler is a running collection loop.
type profiler struct {
	cfg       *config
	http      *http.Client
	hostname  string
	runtimeID string
	seq       int
	cancel    context.CancelFunc
	done      chan struct{}
}

var (
	mu     sync.Mutex
	active *profiler
)

// Start begins collecting and uploading profiles on a background goroutine. A
// second Start while one is running is a no-op.
func Start(opts ...Option) error {
	cfg := newConfig(opts)
	mu.Lock()
	defer mu.Unlock()
	if active != nil {
		return nil
	}
	if cfg.mutexFraction > 0 {
		runtime.SetMutexProfileFraction(cfg.mutexFraction)
	}
	if cfg.blockRate > 0 {
		runtime.SetBlockProfileRate(cfg.blockRate)
	}
	host, _ := os.Hostname()
	p := &profiler{
		cfg:       cfg,
		http:      &http.Client{Timeout: 30 * time.Second},
		hostname:  host,
		runtimeID: newUUID(),
		done:      make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	active = p
	go func() {
		defer close(p.done)
		p.run(ctx)
	}()
	cfg.log.Debug("profiler started", "service", cfg.service, "period", cfg.period, "url", cfg.url)
	return nil
}

// Stop cancels the profiler, abandoning any in-flight collection or upload, and
// returns once its goroutine has exited.
func Stop() {
	mu.Lock()
	p := active
	active = nil
	mu.Unlock()
	if p == nil {
		return
	}
	p.cancel()
	<-p.done
}

func (p *profiler) run(ctx context.Context) {
	for {
		bat, err := p.collect(ctx)
		if err != nil { // cancelled mid-cycle
			return
		}
		if err := p.upload(ctx, bat); err != nil {
			if !errors.Is(err, context.Canceled) { // Stop mid-upload is not a failure
				p.cfg.log.Warn("profile upload failed", "err", err)
			}
		} else {
			p.cfg.log.Debug("profile uploaded", "seq", p.seq, "attachments", len(bat.profiles))
		}
		p.seq++
	}
}

type attachment struct {
	name string
	data []byte
}

type batch struct {
	start, end time.Time
	profiles   []attachment
}

// traceLimit caps one period's execution trace. A busy program can emit trace
// data far faster than any other profile grows, so the trace is stopped early
// once this many bytes are buffered and the partial trace uploads as usual
// (dd-trace-go bounds its trace at the same size).
const traceLimit = 5 << 20

// traceWriter buffers the execution trace and closes full once limit bytes are
// buffered. It does not stop the trace itself: Write runs on the trace reader's
// goroutine and trace.Stop waits for that goroutine, so the collect loop
// receives on full and stops the trace from its side.
type traceWriter struct {
	buf    bytes.Buffer
	limit  int
	full   chan struct{}
	closed bool
}

func (w *traceWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if w.buf.Len() >= w.limit && !w.closed {
		w.closed = true
		close(w.full)
	}
	return n, err
}

// collect runs one profiling cycle: it starts the period-spanning profiles (CPU and
// the execution trace), waits one period (stopping the trace early if it reaches
// traceLimit), then snapshots the instantaneous profiles. It returns ctx.Err() if
// the profiler was stopped during the wait.
func (p *profiler) collect(ctx context.Context) (batch, error) {
	var bat batch
	bat.start = time.Now()

	var cpuBuf bytes.Buffer
	tw := &traceWriter{limit: traceLimit, full: make(chan struct{})}
	var cpuOn, traceOn bool
	for _, t := range p.cfg.types {
		switch t {
		case CPUProfile:
			cpuOn = pprof.StartCPUProfile(&cpuBuf) == nil
		case ExecutionTrace:
			traceOn = trace.Start(tw) == nil
		}
	}

	// The wait spans one period. A trace that fills its budget is stopped there
	// and kept, while the CPU profile keeps running to the period boundary.
	running := traceOn
	stopTrace := func() {
		if running {
			trace.Stop()
			running = false
		}
	}
	var full <-chan struct{}
	if traceOn {
		full = tw.full
	}
	deadline := time.After(p.cfg.period)
	for wait := true; wait; {
		select {
		case <-ctx.Done():
			if cpuOn {
				pprof.StopCPUProfile()
			}
			stopTrace()
			return batch{}, ctx.Err()
		case <-full:
			stopTrace()
			full = nil
		case <-deadline:
			wait = false
		}
	}

	if cpuOn {
		pprof.StopCPUProfile()
	}
	stopTrace()
	bat.end = time.Now()

	for _, t := range p.cfg.types {
		switch t {
		case CPUProfile:
			if cpuOn {
				bat.profiles = append(bat.profiles, attachment{t.filename(), cpuBuf.Bytes()})
			}
		case ExecutionTrace:
			if traceOn {
				bat.profiles = append(bat.profiles, attachment{t.filename(), tw.buf.Bytes()})
			}
		default:
			// pprof.Lookup(name).WriteTo(_, 0) writes the gzip-compressed protobuf
			// the intake wants, so the bytes are the upload-ready attachment.
			pr := pprof.Lookup(t.lookup())
			if pr == nil {
				continue
			}
			var buf bytes.Buffer
			if pr.WriteTo(&buf, 0) == nil {
				bat.profiles = append(bat.profiles, attachment{t.filename(), buf.Bytes()})
			}
		}
	}
	return bat, nil
}

// uploadEvent is the event.json the profiler attaches, the shape the trace-agent
// proxy forwards and the backend reads.
type uploadEvent struct {
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Attachments []string `json:"attachments"`
	Tags        string   `json:"tags_profiler"`
	Family      string   `json:"family"`
	Version     string   `json:"version"`
	Info        struct {
		Profiler struct {
			Activation string `json:"activation"`
			SSI        struct {
				Mechanism string `json:"mechanism"`
			} `json:"ssi"`
			Settings map[string]any `json:"settings"`
		} `json:"profiler"`
	} `json:"info"`
}

func (p *profiler) upload(ctx context.Context, bat batch) error {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	var names []string
	for _, a := range bat.profiles {
		f, err := mw.CreateFormFile(a.name, a.name)
		if err != nil {
			return err
		}
		if _, err := f.Write(a.data); err != nil {
			return err
		}
		names = append(names, a.name)
	}

	ev := uploadEvent{
		Start:       bat.start.Format(time.RFC3339Nano),
		End:         bat.end.Format(time.RFC3339Nano),
		Attachments: names,
		Tags:        p.tagsProfiler(),
		Family:      "go",
		Version:     "4",
	}
	ev.Info.Profiler.Activation = "manual"
	ev.Info.Profiler.SSI.Mechanism = "none"
	ev.Info.Profiler.Settings = map[string]any{}

	ef, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="event"; filename="event.json"`},
		"Content-Type":        {"application/json"},
	})
	if err != nil {
		return err
	}
	if err := json.NewEncoder(ef).Encode(ev); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("DD-EVP-ORIGIN", "dd-micro-agent")

	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}

// tagsProfiler builds the comma-joined tags_profiler string, the same shape
// dd-trace-go sends, so the profile lands tagged in the org.
func (p *profiler) tagsProfiler() string {
	tags := []string{
		"service:" + p.cfg.service,
		"runtime:go",
		"language:go",
		"runtime-id:" + p.runtimeID,
		"profile_seq:" + strconv.Itoa(p.seq),
	}
	if p.cfg.env != "" {
		tags = append(tags, "env:"+p.cfg.env)
	}
	if p.cfg.version != "" {
		tags = append(tags, "version:"+p.cfg.version)
	}
	if p.hostname != "" {
		tags = append(tags, "host:"+p.hostname)
	}
	return strings.Join(append(tags, p.cfg.tags...), ",")
}

// newUUID returns a random RFC 4122 version-4 UUID, the runtime-id format the
// backend keys a profile on.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
