package host

// Disk I/O (system.io.*) on DragonFly from the kern.devstat.all sysctl, the kernel's
// per-device cumulative counters (the same source iostat reads), diffed between reads into
// rates. SysctlRaw is the same call style as bsdCPU's kern.cp_time. The binary decode lives
// in the neutral devstatdf.go (unit-tested on the dev host).

import (
	"time"

	"golang.org/x/sys/unix"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

type dragonflyIO struct {
	prev   map[string]devstatRec
	prevTs time.Time
}

func (*dragonflyIO) name() string { return "diskio" }

func (c *dragonflyIO) collect(now time.Time) ([]metrics.Serie, error) {
	b, err := unix.SysctlRaw("kern.devstat.all")
	if err != nil {
		return nil, err
	}
	cur := make(map[string]devstatRec)
	for _, r := range parseDevstatDF(b) {
		cur[r.name] = r
	}

	var out []metrics.Serie
	if c.prev != nil {
		if dt := now.Sub(c.prevTs).Seconds(); dt > 0 {
			for name, r := range cur {
				if p, ok := c.prev[name]; ok {
					out = append(out, devstatDFSeries(name, r, p, dt, now)...)
				}
			}
		}
	}
	c.prev, c.prevTs = cur, now
	return out, nil
}
