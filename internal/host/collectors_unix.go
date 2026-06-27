//go:build darwin || freebsd || openbsd || netbsd || dragonfly

package host

import (
	"encoding/binary"
	"runtime"
	"time"

	"golang.org/x/sys/unix"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// load: vm.loadavg (struct loadavg{ uint32 ldavg[3]; long fscale })

type bsdLoad struct{}

func (bsdLoad) name() string { return "load" }

func (bsdLoad) collect(now time.Time) ([]metrics.Serie, error) {
	b, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		return nil, err
	}
	if len(b) < 24 { // 3*uint32 + pad + long, on 64-bit
		return nil, nil
	}
	scale := float64(binary.LittleEndian.Uint64(b[16:24]))
	if scale == 0 {
		scale = 2048 // FSCALE fallback (1<<FSHIFT)
	}
	ld := [3]float64{}
	for i := 0; i < 3; i++ {
		ld[i] = float64(binary.LittleEndian.Uint32(b[i*4:])) / scale
	}
	n := float64(runtime.NumCPU())
	if n == 0 {
		n = 1
	}
	return []metrics.Serie{
		gauge("system.load.1", now, ld[0]),
		gauge("system.load.5", now, ld[1]),
		gauge("system.load.15", now, ld[2]),
		gauge("system.load.norm.1", now, ld[0]/n),
		gauge("system.load.norm.5", now, ld[1]/n),
		gauge("system.load.norm.15", now, ld[2]/n),
	}, nil
}

// uptime: kern.boottime

type bsdUptime struct{}

func (bsdUptime) name() string { return "uptime" }

func (bsdUptime) collect(now time.Time) ([]metrics.Serie, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return nil, err
	}
	up := now.Unix() - int64(tv.Sec)
	if up < 0 {
		return nil, nil
	}
	return []metrics.Serie{gauge("system.uptime", now, float64(up))}, nil
}

// blocksAvail clamps a possibly negative available-block count to zero. A
// filesystem filled past its root reserve reports negative Bavail, and the
// unsigned conversion would otherwise wrap it to exabytes.
func blocksAvail(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// sysctlNum reads a numeric sysctl as a little-endian 4- or 8-byte integer.
func sysctlNum(name string) uint64 {
	b, err := unix.SysctlRaw(name)
	if err != nil {
		return 0
	}
	switch {
	case len(b) >= 8:
		return binary.LittleEndian.Uint64(b[:8])
	case len(b) >= 4:
		return uint64(binary.LittleEndian.Uint32(b[:4]))
	default:
		return 0
	}
}
