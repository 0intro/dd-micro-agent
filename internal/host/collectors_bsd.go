//go:build freebsd || openbsd || netbsd || dragonfly

package host

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"time"

	"golang.org/x/sys/unix"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// bsdCommon returns the sub-collectors that work uniformly across the BSDs:
// cpu via kern.cp_time (here) plus load/uptime (collectors_unix.go). Per-OS
// files append memory/disk where implemented.
func bsdCommon() []subCollector {
	return []subCollector{&bsdCPU{}, &bsdLoad{}, &bsdUptime{}}
}

// cpu: kern.cp_time, diffed to %. FreeBSD and NetBSD have five states
// (user, nice, sys, intr, idle). OpenBSD has six since 6.4 (user, nice, sys,
// spin, intr, idle). Idle is always last and everything between nice and idle
// is system time, so the split follows the array length.

type bsdCPU struct {
	prev []uint64
}

func (c *bsdCPU) name() string { return "cpu" }

func (c *bsdCPU) collect(now time.Time) ([]metrics.Serie, error) {
	b, err := unix.SysctlRaw("kern.cp_time")
	if err != nil {
		return nil, err
	}
	n := len(b) / 8
	if n < 5 {
		return nil, fmt.Errorf("kern.cp_time: %d states, want at least 5", n)
	}
	cur := make([]uint64, n)
	for i := range cur {
		cur[i] = binary.LittleEndian.Uint64(b[i*8:])
	}

	out := []metrics.Serie{gauge("system.cpu.num_cores", now, float64(runtime.NumCPU()))}
	if len(c.prev) == n {
		d := make([]uint64, n)
		var total uint64
		for i := range d {
			d[i] = cur[i] - c.prev[i]
			total += d[i]
		}
		if total > 0 {
			var system uint64
			for _, v := range d[2 : n-1] { // sys plus any spin/intr states
				system += v
			}
			pct := func(x uint64) float64 { return float64(x) / float64(total) * 100 }
			out = append(out,
				gauge("system.cpu.user", now, pct(d[0]+d[1])), // user + nice
				gauge("system.cpu.system", now, pct(system)),
				gauge("system.cpu.idle", now, pct(d[n-1])),
			)
		}
	}
	c.prev = cur
	return out, nil
}
