package host

// parseDevstatDF decodes a DragonFly kern.devstat.all sysctl blob: an 8-byte generation
// counter followed by an array of fixed-size struct devstat (sys/sys/devicestat.h, version
// 4). DragonFly's struct differs from FreeBSD's: named scalar counters (bytes_read and so
// on) rather than arrays, and no per-transaction duration array, so the awaits FreeBSD
// emits are unavailable. Its busy_time is not a usable busy accumulator either (it reads
// near zero or negative on a disk that has moved real data), so util is left out too, and
// DragonFly reports the four throughput rates only. The offsets are pinned for the amd64
// ABI and verified against DragonFly 6.4.x (sizeof 200) by the vm_dragonfly e2e, which
// dumps the layout plus a live sample. Neutral (no build tag) so the parser unit-tests on
// the dev host, io_dragonfly.go (dragonfly-only) makes the sysctl call.

import (
	"encoding/binary"
	"strconv"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

const (
	dfDevstatGen    = 8   // leading generation counter (the blob is 8 + n*200 bytes)
	dfDevstatStride = 200 // sizeof(struct devstat), amd64
	dfDsName        = 12  // char device_name[16] (driver class, e.g. "vbd"/"cd")
	dfDsUnit        = 28  // int unit_number (the trailing number, full device = name+unit)
	dfDsBytesRead   = 32  // u_int64_t bytes_read
	dfDsBytesWrite  = 40  // u_int64_t bytes_written
	dfDsNumReads    = 56  // u_int64_t num_reads
	dfDsNumWrites   = 64  // u_int64_t num_writes
)

func parseDevstatDF(b []byte) []devstatRec {
	var out []devstatRec
	for off := dfDevstatGen; off+dfDevstatStride <= len(b); off += dfDevstatStride {
		s := b[off : off+dfDevstatStride]
		unit := int32(binary.LittleEndian.Uint32(s[dfDsUnit:]))
		out = append(out, devstatRec{
			name:       trimNul(s[dfDsName:dfDsName+16]) + strconv.Itoa(int(unit)),
			readOps:    u64f(s, dfDsNumReads),
			writeOps:   u64f(s, dfDsNumWrites),
			readBytes:  u64f(s, dfDsBytesRead),
			writeBytes: u64f(s, dfDsBytesWrite),
		})
	}
	return out
}

// devstatDFSeries computes the iostat-style throughput rates for one device over [p, c] /
// dt. DragonFly devstat keeps no per-operation durations and no usable busy time, so it
// emits neither the awaits nor util that FreeBSD adds.
func devstatDFSeries(name string, c, p devstatRec, dt float64, now time.Time) []metrics.Serie {
	dRead, dWrite := c.readOps-p.readOps, c.writeOps-p.writeOps
	if dRead < 0 || dWrite < 0 {
		return nil // counter reset or device replaced
	}
	dev := "device:" + name
	return []metrics.Serie{
		gauge("system.io.r_s", now, dRead/dt, dev),
		gauge("system.io.w_s", now, dWrite/dt, dev),
		gauge("system.io.rkb_s", now, nonneg(c.readBytes-p.readBytes)/1024/dt, dev),
		gauge("system.io.wkb_s", now, nonneg(c.writeBytes-p.writeBytes)/1024/dt, dev),
	}
}
