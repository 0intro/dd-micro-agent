// Package process implements the process agent: it collects the running
// processes and ships them to the Live Processes intake. That intake speaks
// protobuf, not JSON, so this package hand-rolls the wire format (see proto.go /
// payload.go), keeping the agent free of a protobuf library, cgo, and zstd.
//
// Like internal/venti it is opt-in (process_config.process_collection.enabled)
// and runs on its own goroutine. It mirrors the stock agent's two-stream model:
// a full process list (CollectorProc) every Interval, plus a lighter realtime
// stats stream (CollectorRealTime) every few seconds, the latter switched on
// only while someone is viewing the page, which the intake signals back in each
// response (see applyRT).
//
// Per-process collection is OS-specific: each collect_<goos>.go provides
// collectProcs. Operating systems without an implementation (DragonFly, Solaris,
// and so on) get the collect_other.go stub and simply report nothing.
package process

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

// additionalTimeout caps how long each best-effort (non-primary) endpoint may take,
// so a dead secondary org cannot stall the realtime stream's tight cadence.
const additionalTimeout = 5 * time.Second

// maxPerMessage bounds how many processes ride in one CollectorProc chunk,
// matching the stock Agent's process_config.max_per_message default.
const maxPerMessage = 100

// Options configures a Reporter.
type Options struct {
	Endpoints         []intake.Endpoint // process intake (primary + additional)
	Hostname          string
	Proxy             string
	SkipSSLValidation bool
	Interval          time.Duration // full process-list interval. Defaults to 10s
	Logger            *slog.Logger
}

// Reporter collects and submits process payloads on its own goroutine.
type Reporter struct {
	sub       *submitter
	endpoints []intake.Endpoint // process intake (primary + additional)
	hostname  string
	totalMem  int64 // host RAM in bytes, read once at startup (static). 0 if unavailable
	log       *slog.Logger

	baseTick     time.Duration // finest cadence (the realtime floor)
	procInterval time.Duration // between full process lists
	rtInterval   time.Duration // between realtime stats (intake may revise it)
	rtEnabled    bool          // realtime on while clients view the page
	lastProc     time.Time
	lastRT       time.Time

	// per-PID previous cumulative counters, for CPU% and IO rates
	prev    map[int32]prevSample
	prevTs  time.Time
	groupID int32
}

// prevSample is the slice of a Proc we diff against the next collection.
type prevSample struct {
	user, sys                                    float64 // cumulative CPU seconds
	readBytes, writeBytes, readCount, writeCount uint64
	createTime                                   int64 // guards against PID reuse
}

// New returns a Reporter. The caller runs it only when process collection is
// enabled, so New does no enablement check.
func New(o Options) *Reporter {
	if o.Interval == 0 {
		o.Interval = 10 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Reporter{
		sub:          newSubmitter(o),
		endpoints:    o.Endpoints,
		hostname:     o.Hostname,
		totalMem:     hostTotalMemory(),
		log:          o.Logger,
		baseTick:     2 * time.Second,
		procInterval: o.Interval,
		rtInterval:   2 * time.Second,
		prev:         map[int32]prevSample{},
		groupID:      int32(time.Now().Unix()),
	}
}

// Run sends a process list at startup, then ticks until ctx is cancelled.
func (r *Reporter) Run(ctx context.Context) {
	r.maybeSend(ctx, true)
	t := time.NewTicker(r.baseTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.maybeSend(ctx, false)
		}
	}
}

// maybeSend decides what (if anything) this tick owes: a full process list when
// the process interval has elapsed (or at startup), otherwise a realtime update
// when realtime is enabled and its interval has elapsed. A process send also
// resets the realtime clock so the two streams don't double up on one tick.
func (r *Reporter) maybeSend(ctx context.Context, startup bool) {
	now := time.Now()
	switch {
	case startup || now.Sub(r.lastProc) >= r.procInterval:
		r.send(ctx, now, false)
		r.lastProc, r.lastRT = now, now
	case r.rtEnabled && now.Sub(r.lastRT) >= r.rtInterval:
		r.send(ctx, now, true)
		r.lastRT = now
	}
}

// send collects, rate-fills, chunks, and submits one payload. realtime selects
// the lighter CollectorRealTime stream over the full CollectorProc list.
func (r *Reporter) send(ctx context.Context, now time.Time, realtime bool) {
	procs, err := collectProcs()
	if err != nil {
		r.log.Warn("process collection failed", "err", err)
		return
	}
	samples := r.rate(procs, now)
	if len(samples) == 0 {
		return
	}
	info := r.sysInfo()
	chunks := chunk(samples, maxPerMessage)
	gid := r.groupID
	r.groupID++
	sent := 0
	for _, ch := range chunks {
		var framed []byte
		if realtime {
			framed = frame(typeCollectorRealTime,
				encodeCollectorRealTime(r.hostname, ch, gid, int32(len(chunks)), info.numCPU, info.totalMemory))
		} else {
			framed = frame(typeCollectorProc,
				encodeCollectorProc(r.hostname, info, ch, gid, int32(len(chunks))))
		}
		// The primary gates success and drives the realtime toggle. Additional
		// endpoints are best-effort: their responses (and failures) are ignored.
		resp, err := r.sub.post(ctx, r.endpoints[0], framed)
		if err != nil {
			r.log.Warn("process payload send failed", "realtime", realtime, "err", err)
			continue
		}
		sent++
		r.applyRT(resp)
		for _, ep := range r.endpoints[1:] {
			r.postAdditional(ctx, ep, framed)
		}
	}
	if sent > 0 {
		r.log.Debug("process payload sent", "realtime", realtime, "processes", len(samples), "chunks", sent)
	}
}

// createTimeSlackMs is how far a re-observed PID's create time may move before we
// treat it as a different process (PID reuse) rather than the same one. A real
// process's create time is fixed. Platforms that can only derive it as now-age
// (Plan 9) wobble by a clock tick, well under this, while PID reuse jumps by the
// dead process's whole lifetime.
const createTimeSlackMs = 2000

// rate fills each process's CPU percentages and IO rates by diffing this
// collection's cumulative counters against the previous one, then records the new
// values for next time. The first collection (or a PID newly seen, or a reused
// PID, detected by a changed create time) yields no rates.
func (r *Reporter) rate(procs []Proc, now time.Time) []*Proc {
	dt := now.Sub(r.prevTs).Seconds()
	cur := make(map[int32]prevSample, len(procs))
	out := make([]*Proc, 0, len(procs))
	for i := range procs {
		p := procs[i] // copy. We hand out a pointer to the copy
		// Pin the create time to the first value seen for this PID. The intake keys
		// process identity on (pid, createTime), so an unstable create time makes the
		// backend treat every snapshot as a brand-new process and retain almost none.
		// And it would also defeat the diff below. Reuse the prior value whenever
		// it's close enough to be the same process.
		if old, ok := r.prev[p.Pid]; ok && abs64(old.createTime-p.CreateTime) < createTimeSlackMs {
			p.CreateTime = old.createTime
		}
		cur[p.Pid] = prevSample{
			user: p.UserTime, sys: p.SystemTime,
			readBytes: p.ReadBytes, writeBytes: p.WriteBytes,
			readCount: p.ReadCount, writeCount: p.WriteCount,
			createTime: p.CreateTime,
		}
		if dt > 0 {
			if old, ok := r.prev[p.Pid]; ok && old.createTime == p.CreateTime {
				p.UserPct = pct(p.UserTime, old.user, dt)
				p.SystemPct = pct(p.SystemTime, old.sys, dt)
				p.TotalPct = p.UserPct + p.SystemPct
				p.ReadBytesRate = rate(p.ReadBytes, old.readBytes, dt)
				p.WriteBytesRate = rate(p.WriteBytes, old.writeBytes, dt)
				p.ReadRate = rate(p.ReadCount, old.readCount, dt)
				p.WriteRate = rate(p.WriteCount, old.writeCount, dt)
			}
		}
		out = append(out, &p)
	}
	r.prev, r.prevTs = cur, now
	return out
}

// postAdditional ships one framed payload to a best-effort endpoint under a short
// deadline, logging and discarding any failure or response.
func (r *Reporter) postAdditional(ctx context.Context, ep intake.Endpoint, framed []byte) {
	ctx, cancel := context.WithTimeout(ctx, additionalTimeout)
	defer cancel()
	if _, err := r.sub.post(ctx, ep, framed); err != nil {
		r.log.Warn("process payload send to additional endpoint failed", "url", ep.URL, "err", err)
	}
}

// applyRT updates the realtime toggle and interval from an intake response.
func (r *Reporter) applyRT(resp []byte) {
	active, interval, ok := readResCollector(resp)
	if !ok {
		return
	}
	r.rtEnabled = active > 0
	if interval > 0 {
		r.rtInterval = time.Duration(interval) * time.Second
	}
}

func (r *Reporter) sysInfo() sysInfo {
	return sysInfo{os: runtime.GOOS, platform: runtime.GOOS, numCPU: runtime.NumCPU(), totalMemory: r.totalMem}
}

// pct is a CPU-time delta as a percentage of wall time (seconds over seconds).
func pct(cur, old, dt float64) float32 {
	if cur <= old || dt <= 0 {
		return 0
	}
	return float32((cur - old) / dt * 100)
}

// rate is a counter delta per second.
func rate(cur, old uint64, dt float64) float32 {
	if cur <= old || dt <= 0 {
		return 0
	}
	return float32(float64(cur-old) / dt)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// chunk splits ps into runs of at most n. An empty input yields no chunks.
func chunk(ps []*Proc, n int) [][]*Proc {
	var out [][]*Proc
	for i := 0; i < len(ps); i += n {
		out = append(out, ps[i:min(i+n, len(ps))])
	}
	return out
}
