package host

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// collectors returns the Linux sub-collectors, which read from /proc and statfs.
func collectors(c *Collector) []subCollector {
	ncpu := c.numCPU()
	return []subCollector{
		&cpuCollector{c: c},
		&memCollector{c: c},
		&loadCollector{c: c, ncpu: ncpu},
		&uptimeCollector{c: c},
		&fsCollector{c: c, statfs: statfs},
		&netCollector{c: c},
		&ioCollector{c: c},
	}
}

func (c *Collector) readProc(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(c.proc, name))
}

// numCPU counts the per-core "cpuN" lines in /proc/stat (via parseCPU, the one
// parser of that file), falling back to the runtime's view.
func (c *Collector) numCPU() int {
	if data, err := c.readProc("stat"); err == nil {
		if _, n, err := parseCPU(data); err == nil && n > 0 {
			return n
		}
	}
	return runtime.NumCPU()
}

// memory: /proc/meminfo, reported in MB (kB / 1024)

type memCollector struct{ c *Collector }

func (m *memCollector) name() string { return "memory" }

func (m *memCollector) collect(now time.Time) ([]metrics.Serie, error) {
	data, err := m.c.readProc("meminfo")
	if err != nil {
		return nil, err
	}
	kb := parseMeminfo(data)
	mb := func(key string) float64 { return kb[key] / 1024 }

	out := []metrics.Serie{
		gauge("system.mem.total", now, mb("MemTotal")),
		gauge("system.mem.free", now, mb("MemFree")),
		gauge("system.mem.used", now, (kb["MemTotal"]-kb["MemFree"])/1024),
		gauge("system.mem.usable", now, mb("MemAvailable")),
		// Cached plus SReclaimable is the stock Agent's (gopsutil's) cached.
		gauge("system.mem.cached", now, mb("Cached")+mb("SReclaimable")),
		gauge("system.mem.buffered", now, mb("Buffers")),
		gauge("system.mem.shared", now, mb("Shmem")),
		gauge("system.mem.slab", now, mb("Slab")),
		gauge("system.swap.total", now, mb("SwapTotal")),
		gauge("system.swap.free", now, mb("SwapFree")),
		gauge("system.swap.used", now, (kb["SwapTotal"]-kb["SwapFree"])/1024),
	}
	if total := kb["MemTotal"]; total > 0 {
		out = append(out, gauge("system.mem.pct_usable", now, kb["MemAvailable"]/total))
	}
	if st := kb["SwapTotal"]; st > 0 {
		out = append(out, gauge("system.swap.pct_free", now, kb["SwapFree"]/st)) // 0 to 1 ratio
	}
	return out, nil
}

func parseMeminfo(data []byte) map[string]float64 {
	m := make(map[string]float64)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		if v, err := strconv.ParseFloat(f[1], 64); err == nil {
			m[strings.TrimSuffix(f[0], ":")] = v // kB
		}
	}
	return m
}

// load: /proc/loadavg

type loadCollector struct {
	c    *Collector
	ncpu int
}

func (l *loadCollector) name() string { return "load" }

func (l *loadCollector) collect(now time.Time) ([]metrics.Serie, error) {
	data, err := l.c.readProc("loadavg")
	if err != nil {
		return nil, err
	}
	f := strings.Fields(string(data))
	if len(f) < 3 {
		return nil, nil
	}
	load := make([]float64, 3)
	for i := range load {
		load[i], _ = strconv.ParseFloat(f[i], 64)
	}
	n := float64(l.ncpu)
	if n == 0 {
		n = 1
	}
	return []metrics.Serie{
		gauge("system.load.1", now, load[0]),
		gauge("system.load.5", now, load[1]),
		gauge("system.load.15", now, load[2]),
		gauge("system.load.norm.1", now, load[0]/n),
		gauge("system.load.norm.5", now, load[1]/n),
		gauge("system.load.norm.15", now, load[2]/n),
	}, nil
}

// uptime: /proc/uptime

type uptimeCollector struct{ c *Collector }

func (u *uptimeCollector) name() string { return "uptime" }

func (u *uptimeCollector) collect(now time.Time) ([]metrics.Serie, error) {
	data, err := u.c.readProc("uptime")
	if err != nil {
		return nil, err
	}
	f := strings.Fields(string(data))
	if len(f) < 1 {
		return nil, nil
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return nil, err
	}
	return []metrics.Serie{gauge("system.uptime", now, secs)}, nil
}
