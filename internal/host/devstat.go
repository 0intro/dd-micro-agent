package host

// parseDevstat decodes a FreeBSD `kern.devstat.all` sysctl blob, an 8-byte generation
// counter followed by an array of fixed-size `struct devstat` (sys/sys/devicestat.h).
// The layout is pinned for the amd64 ABI and verified against FreeBSD 15.1 (devstat
// version 6, sizeof 288) by the vm_freebsd e2e, which dumps the header + a live sample.
// The offsets are stable across the 1x.y releases. This file carries no build tag so the
// parser unit-tests on the dev host, io_freebsd.go (freebsd-only) makes the sysctl call.

import (
	"bytes"
	"encoding/binary"
	"math"
	"strconv"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

const (
	devstatGen    = 8   // leading generation counter
	devstatStride = 288 // sizeof(struct devstat), amd64
	// field offsets within one struct devstat
	dsName     = 44  // char device_name[16] (driver class, e.g. "vtbd"/"cd")
	dsUnit     = 60  // int unit_number (the trailing number, full device = name+unit)
	dsBytes    = 64  // u_int64_t bytes[4]
	dsOps      = 96  // u_int64_t operations[4]
	dsDur      = 128 // struct bintime duration[4] (16 bytes each)
	dsBusy     = 192 // struct bintime busy_time
	transRead  = 1   // enum devstat_trans_flags: NO_DATA=0, READ=1, WRITE=2, FREE=3
	transWrite = 2
)

// devstatRec is one device's cumulative counters (durations already in seconds).
type devstatRec struct {
	name                       string
	readOps, writeOps          float64
	readBytes, writeBytes      float64
	readDurS, writeDurS, busyS float64
}

func parseDevstat(b []byte) []devstatRec {
	var out []devstatRec
	for off := devstatGen; off+devstatStride <= len(b); off += devstatStride {
		s := b[off : off+devstatStride]
		unit := int32(binary.LittleEndian.Uint32(s[dsUnit:]))
		out = append(out, devstatRec{
			name:       trimNul(s[dsName:dsName+16]) + strconv.Itoa(int(unit)),
			readOps:    u64f(s, dsOps+8*transRead),
			writeOps:   u64f(s, dsOps+8*transWrite),
			readBytes:  u64f(s, dsBytes+8*transRead),
			writeBytes: u64f(s, dsBytes+8*transWrite),
			readDurS:   bintimeSec(s, dsDur+16*transRead),
			writeDurS:  bintimeSec(s, dsDur+16*transWrite),
			busyS:      bintimeSec(s, dsBusy),
		})
	}
	return out
}

// devstatSeries computes the iostat-style rates for one device over [p, c] / dt. It is
// neutral (called only by the freebsd collector) so the rate math unit-tests on the dev
// host. Durations are seconds, converted to per-op ms for the awaits.
func devstatSeries(name string, c, p devstatRec, dt float64, now time.Time) []metrics.Serie {
	dRead, dWrite := c.readOps-p.readOps, c.writeOps-p.writeOps
	if dRead < 0 || dWrite < 0 {
		return nil // counter reset / device replaced
	}
	dev := "device:" + name
	out := []metrics.Serie{
		gauge("system.io.r_s", now, dRead/dt, dev),
		gauge("system.io.w_s", now, dWrite/dt, dev),
		gauge("system.io.rkb_s", now, nonneg(c.readBytes-p.readBytes)/1024/dt, dev),
		gauge("system.io.wkb_s", now, nonneg(c.writeBytes-p.writeBytes)/1024/dt, dev),
	}
	util := nonneg(c.busyS-p.busyS) / dt * 100 // busy-time fraction → %
	if util > 100 {
		util = 100
	}
	out = append(out, gauge("system.io.util", now, util, dev))
	dRdMs, dWrMs := nonneg(c.readDurS-p.readDurS)*1000, nonneg(c.writeDurS-p.writeDurS)*1000
	if ops := dRead + dWrite; ops > 0 {
		out = append(out, gauge("system.io.await", now, (dRdMs+dWrMs)/ops, dev))
	}
	if dRead > 0 {
		out = append(out, gauge("system.io.r_await", now, dRdMs/dRead, dev))
	}
	if dWrite > 0 {
		out = append(out, gauge("system.io.w_await", now, dWrMs/dWrite, dev))
	}
	return out
}

func u64f(s []byte, o int) float64 { return float64(binary.LittleEndian.Uint64(s[o:])) }

// bintimeSec decodes `struct bintime { int64 sec; uint64 frac }` at offset o to seconds.
func bintimeSec(s []byte, o int) float64 {
	sec := int64(binary.LittleEndian.Uint64(s[o:]))
	frac := binary.LittleEndian.Uint64(s[o+8:])
	return float64(sec) + math.Ldexp(float64(frac), -64) // frac / 2^64
}

func trimNul(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// nonneg clamps a counter delta to ≥0 (a negative delta means a counter reset). Lives
// here (neutral) so both io_linux.go and io_freebsd.go can use it.
func nonneg(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// ratio is num/den, guarding division by zero. Lives here (neutral) so both disk
// collectors (fs_linux.go and collectors_diskbsd.go) share one definition.
func ratio(num, den float64) float64 {
	if den == 0 {
		return 0
	}
	return num / den
}
