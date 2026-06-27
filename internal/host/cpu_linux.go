package host

import (
	"bufio"
	"bytes"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// cpuCollector turns the cumulative jiffy counters in /proc/stat into the usual
// CPU-time percentages by diffing against the previous read. The first read only
// establishes the baseline (and reports the core count). Percentages appear from
// the second read on.
type cpuCollector struct {
	c    *Collector
	prev cpuTimes
	has  bool
}

type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal, guest float64
}

// total is the denominator for percentages. Guest time is already counted in
// user, so it is excluded to avoid double-counting (matching the stock check).
func (t cpuTimes) total() float64 {
	return t.user + t.nice + t.system + t.idle + t.iowait + t.irq + t.softirq + t.steal
}

func (cc *cpuCollector) name() string { return "cpu" }

func (cc *cpuCollector) collect(now time.Time) ([]metrics.Serie, error) {
	data, err := cc.c.readProc("stat")
	if err != nil {
		return nil, err
	}
	cur, ncpu, err := parseCPU(data)
	if err != nil {
		return nil, err
	}

	out := []metrics.Serie{gauge("system.cpu.num_cores", now, float64(ncpu))}

	if cc.has {
		if dt := cur.total() - cc.prev.total(); dt > 0 {
			pct := func(d float64) float64 { return d / dt * 100 }
			out = append(out,
				gauge("system.cpu.user", now, pct((cur.user-cc.prev.user)+(cur.nice-cc.prev.nice))),
				gauge("system.cpu.system", now, pct((cur.system-cc.prev.system)+(cur.irq-cc.prev.irq)+(cur.softirq-cc.prev.softirq))),
				gauge("system.cpu.iowait", now, pct(cur.iowait-cc.prev.iowait)),
				gauge("system.cpu.idle", now, pct(cur.idle-cc.prev.idle)),
				gauge("system.cpu.stolen", now, pct(cur.steal-cc.prev.steal)),
				gauge("system.cpu.guest", now, pct(cur.guest-cc.prev.guest)),
			)
		}
	}
	cc.prev = cur
	cc.has = true
	return out, nil
}

// parseCPU reads the aggregate "cpu" line and counts the per-core "cpuN" lines.
func parseCPU(data []byte) (cpuTimes, int, error) {
	var t cpuTimes
	ncpu := 0
	found := false

	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		f := strings.Fields(line)
		if f[0] == "cpu" {
			n := parseFloats(f[1:])
			if len(n) < 8 {
				return t, 0, errors.New("malformed /proc/stat cpu line")
			}
			t = cpuTimes{
				user: n[0], nice: n[1], system: n[2], idle: n[3],
				iowait: n[4], irq: n[5], softirq: n[6], steal: n[7],
			}
			if len(n) > 8 {
				t.guest = n[8]
			}
			found = true
		} else {
			ncpu++
		}
	}
	if !found {
		return t, 0, errors.New("no cpu line in /proc/stat")
	}
	return t, ncpu, nil
}

func parseFloats(fields []string) []float64 {
	out := make([]float64, len(fields))
	for i, f := range fields {
		out[i], _ = strconv.ParseFloat(f, 64)
	}
	return out
}
