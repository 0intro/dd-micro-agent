package logs

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0intro/dd-micro-agent/internal/config"
	"github.com/0intro/dd-micro-agent/internal/intake"
)

// scanInterval is how often glob log sources are re-expanded to pick up files that
// appear after startup (e.g. a service writing its log for the first time).
const scanInterval = 30 * time.Second

// Agent runs the logs pipeline: one tailer per file feeds a batcher, which posts
// batches to the intake and advances the registry on success.
type Agent struct {
	opts     Options
	registry *Registry
}

// Options configures the logs Agent.
type Options struct {
	Sources             []config.LogSource
	Client              *intake.Client
	Endpoints           []intake.Endpoint // logs intake (primary + additional)
	Hostname            string
	Tags                []string // global tags added to every log
	RunPath             string   // directory for registry.json
	BatchWait           time.Duration
	BatchMaxSize        int
	BatchMaxContentSize int
	PollInterval        time.Duration
	ScanInterval        time.Duration // glob re-expansion period, defaults to scanInterval
	Logger              *slog.Logger
}

// NewAgent returns a logs Agent, loading any existing registry.
func NewAgent(o Options) *Agent {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.BatchWait == 0 {
		o.BatchWait = 5 * time.Second
	}
	if o.BatchMaxSize == 0 {
		o.BatchMaxSize = 1000
	}
	if o.BatchMaxContentSize == 0 {
		o.BatchMaxContentSize = 5_000_000
	}
	if o.PollInterval == 0 {
		o.PollInterval = defaultPollDelay
	}
	if o.ScanInterval == 0 {
		o.ScanInterval = scanInterval
	}
	reg := LoadRegistry(filepath.Join(o.RunPath, "registry.json"), o.Logger)
	return &Agent{opts: o, registry: reg}
}

// Run starts the pipeline and blocks until ctx is cancelled. Shutdown is ordered
// so nothing in flight is lost: tailers stop, the channel closes, the batcher
// drains and sends what remains, then the registry is persisted.
func (a *Agent) Run(ctx context.Context) {
	o := a.opts
	msgs := make(chan Message, 1000)

	regCtx, regCancel := context.WithCancel(context.Background())
	var regWG sync.WaitGroup
	regWG.Add(1)
	go func() {
		defer regWG.Done()
		a.registry.Run(regCtx, time.Second)
	}()

	b := &batcher{
		in:         msgs,
		client:     o.Client,
		endpoints:  o.Endpoints,
		hostname:   o.Hostname,
		globalTags: o.Tags,
		registry:   a.registry,
		maxSize:    o.BatchMaxSize,
		maxBytes:   o.BatchMaxContentSize,
		wait:       o.BatchWait,
		log:        o.Logger,
	}
	batchDone := make(chan struct{})
	go func() {
		b.run(ctx)
		close(batchDone)
	}()

	// Each source path is either a literal file or a glob (e.g. /sys/log/*). A glob
	// expands to the regular files it matches. A literal path is tailed as-is, even
	// if it doesn't exist yet (the tailer waits for it). startTailer is idempotent
	// per absolute path, so re-expanding a glob never double-tails a file, and a
	// glob-discovered file that leaves the match set for two scans has its tailer
	// stopped and its registry entry removed (the grace outlives a logrotate
	// rename gap, so a live rotation never kills a tailer).
	type tailerHandle struct {
		cancel   context.CancelFunc
		fromGlob bool
		missed   int // consecutive scans without a glob match
	}
	var tailWG sync.WaitGroup
	tailed := make(map[string]*tailerHandle) // abs path -> handle, touched only via discover

	startTailer := func(src config.LogSource, abs string, fromGlob bool) {
		if tailed[abs] != nil {
			return
		}
		tctx, cancel := context.WithCancel(ctx)
		tailed[abs] = &tailerHandle{cancel: cancel, fromGlob: fromGlob}
		t := &tailer{
			source:   src,
			id:       "file:" + abs,
			path:     abs,
			out:      msgs,
			registry: a.registry,
			poll:     o.PollInterval,
			log:      o.Logger,
		}
		tailWG.Add(1)
		go func() {
			defer tailWG.Done()
			t.run(tctx)
		}()
		o.Logger.Info("tailing log file", "path", abs, "service", src.Service, "source", src.Source)
	}

	discover := func() {
		matched := make(map[string]bool)
		for _, src := range o.Sources {
			if src.Type == "journald" {
				continue // started once below, not a file path to expand
			}
			abs, err := filepath.Abs(src.Path)
			if err != nil {
				o.Logger.Warn("skipping log source with bad path", "path", src.Path, "err", err)
				continue
			}
			if !strings.ContainsAny(src.Path, "*?[") { // a literal path
				startTailer(src, abs, false)
				continue
			}
			matches, err := filepath.Glob(abs)
			if err != nil {
				o.Logger.Warn("skipping log source with bad glob", "path", src.Path, "err", err)
				continue
			}
			for _, m := range matches {
				if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
					matched[m] = true
					startTailer(src, m, true)
				}
			}
		}
		// Sweep the glob-discovered tailers whose file is gone. Literal paths
		// stay: they are deliberately tailed before they exist. An in-flight
		// acknowledgement may re-create a removed registry entry, which is
		// harmless (the reopen guards handle a stale offset).
		for path, h := range tailed {
			if !h.fromGlob || matched[path] {
				h.missed = 0
				continue
			}
			if h.missed++; h.missed < 2 {
				continue
			}
			h.cancel()
			delete(tailed, path)
			a.registry.Remove("file:" + path)
			o.Logger.Info("stopped tailing (the file left the glob)", "path", path)
		}
	}

	// journald sources have no path to glob and never rotate the way files do, so
	// each starts a single tailer for the agent's life. They join tailWG and msgs,
	// so the ordered shutdown below covers them unchanged.
	for _, src := range o.Sources {
		if src.Type == "journald" {
			startJournald(ctx, src, msgs, a.registry, &tailWG, o.Logger)
		}
	}

	discover() // initial expansion

	// Rescan so globs pick up files that appear later. Only this goroutine calls
	// discover after the initial pass, so tailed/tailWG are never touched
	// concurrently. It stops on ctx.Done before the shutdown drain begins, so no new
	// tailer can start once we're tearing down.
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		ticker := time.NewTicker(o.ScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				discover()
			}
		}
	}()

	<-ctx.Done()
	<-scanDone    // discovery stopped. No more tailers will start
	tailWG.Wait() // tailers have stopped reading and sending
	close(msgs)   // no more messages will arrive
	<-batchDone   // batcher drained the channel and flushed
	regCancel()   // stop periodic flushes
	regWG.Wait()
	a.registry.Flush() // persist the offsets from the final drain
}

// batcher accumulates messages and flushes them on size, byte, or time limits.
type batcher struct {
	in         <-chan Message
	client     *intake.Client
	endpoints  []intake.Endpoint
	hostname   string
	globalTags []string
	registry   *Registry
	maxSize    int
	maxBytes   int
	wait       time.Duration
	log        *slog.Logger
}

// run reads until the input channel closes, then flushes the remainder. A batch
// the intake did not accept is kept and retried on the next tick, never dropped,
// so the registry cannot advance past an undelivered line. While a batch is
// stuck the loop stops reading, the channel fills, and the tailers block:
// bounded backpressure, like the stock Agent. Sends carry their own bounded
// deadline so the shutdown drain still reaches the intake, and a batch still
// undelivered when ctx ends is abandoned unacknowledged, so a restart re-reads
// it from the registry.
func (b *batcher) run(ctx context.Context) {
	ticker := time.NewTicker(b.wait)
	defer ticker.Stop()

	var batch []Message
	bytesAccum := 0
	stuck := false
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if stuck = !b.send(batch); !stuck {
			batch = nil
			bytesAccum = 0
		}
	}

	for {
		if stuck {
			select {
			case <-ctx.Done():
				b.log.Warn("abandoning undelivered log batch at shutdown, a restart re-reads it from the registry", "count", len(batch))
				return
			case <-ticker.C:
				flush()
			}
			continue
		}
		select {
		case m, ok := <-b.in:
			if !ok {
				flush()
				return
			}
			m.Tags = combineTags(m.Tags, b.globalTags)
			if len(batch) > 0 && bytesAccum+len(m.Content) > b.maxBytes {
				flush() // keep the payload under the intake's size cap
			}
			batch = append(batch, m)
			bytesAccum += len(m.Content)
			if len(batch) >= b.maxSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// send posts one batch and reports whether it is settled: delivered (and
// acknowledged in the registry) or refused outright by the intake. A refused
// batch is dropped with a warning but never acknowledged, so a restart re-reads
// it. A transient failure reports false and the caller retries.
func (b *batcher) send(batch []Message) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, err := SendLogs(ctx, b.client, b.endpoints, b.hostname, batch)
	switch {
	case err == nil:
		b.log.Debug("log batch sent", "count", len(batch))
		for _, m := range batch {
			if m.cursor != "" {
				b.registry.AdvanceCursor(m.sourceID, m.cursor)
			} else {
				b.registry.Advance(m.sourceID, m.offset)
			}
		}
		return true
	case intake.Permanent(status):
		b.log.Warn("log batch refused by the intake, dropping it", "count", len(batch), "status", status, "err", err)
		return true
	default:
		b.log.Warn("log batch send failed, will retry", "count", len(batch), "err", err)
		return false
	}
}

// combineTags returns a fresh slice of a followed by b, never aliasing a (whose
// backing array is shared across a source's messages).
func combineTags(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	return append(out, b...)
}
