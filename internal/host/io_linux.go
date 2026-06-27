package host

// Disk I/O (system.io.*) from /proc/diskstats, the cumulative per-device counters,
// diffed between reads into the iostat-style rates the stock Agent's disk-I/O check
// emits. Same rate-collector shape as netCollector (net_linux.go): the first read only
// establishes the baseline.

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// diskstat holds the /proc/diskstats columns we use (Documentation/admin-guide/iostats):
// 3 reads, 4 reads_merged, 5 sectors_read, 6 ms_reading, 7 writes, 8 writes_merged,
// 9 sectors_written, 10 ms_writing, 12 ms_io, 13 weighted_ms_io. Sectors are 512 bytes.
type diskstat struct {
	reads, readsMerged, sectorsRead, msReading      float64
	writes, writesMerged, sectorsWritten, msWriting float64
	msIo, weightedMsIo                              float64
}

// parseDiskstats parses /proc/diskstats into per-device counters, skipping virtual
// loop/ram devices and any malformed line.
func parseDiskstats(data []byte) map[string]diskstat {
	out := make(map[string]diskstat)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 14 {
			continue
		}
		name := f[2]
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
			continue
		}
		var n [14]float64
		ok := true
		for _, i := range [...]int{3, 4, 5, 6, 7, 8, 9, 10, 12, 13} {
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				ok = false
				break
			}
			n[i] = v
		}
		if !ok {
			continue
		}
		out[name] = diskstat{
			reads: n[3], readsMerged: n[4], sectorsRead: n[5], msReading: n[6],
			writes: n[7], writesMerged: n[8], sectorsWritten: n[9], msWriting: n[10],
			msIo: n[12], weightedMsIo: n[13],
		}
	}
	return out
}

type ioCollector struct {
	c      *Collector
	prev   map[string]diskstat
	prevTs time.Time
}

func (*ioCollector) name() string { return "diskio" }

func (ic *ioCollector) collect(now time.Time) ([]metrics.Serie, error) {
	data, err := ic.c.readProc("diskstats")
	if err != nil {
		return nil, err
	}
	cur := parseDiskstats(data)

	var out []metrics.Serie
	if ic.prev != nil {
		if dt := now.Sub(ic.prevTs).Seconds(); dt > 0 {
			for name, c := range cur {
				p, ok := ic.prev[name]
				if !ok {
					continue
				}
				out = append(out, diskIOSeries(name, c, p, dt, now)...)
			}
		}
	}
	ic.prev, ic.prevTs = cur, now
	return out, nil
}

// diskIOSeries computes the iostat-style rates for one device over [p, c] / dt.
func diskIOSeries(name string, c, p diskstat, dt float64, now time.Time) []metrics.Serie {
	dReads, dWrites := c.reads-p.reads, c.writes-p.writes
	if dReads < 0 || dWrites < 0 {
		return nil // counter reset (reboot)
	}
	dRdSect, dWrSect := c.sectorsRead-p.sectorsRead, c.sectorsWritten-p.sectorsWritten
	// The kernel prints the ms counters as 32-bit even on amd64, so a busy
	// device wraps them every few days. Clamp the wrap to one zero interval.
	dRdMs, dWrMs := nonneg(c.msReading-p.msReading), nonneg(c.msWriting-p.msWriting)
	dev := "device:" + name

	out := []metrics.Serie{
		gauge("system.io.r_s", now, dReads/dt, dev),
		gauge("system.io.w_s", now, dWrites/dt, dev),
		gauge("system.io.rrqm_s", now, nonneg(c.readsMerged-p.readsMerged)/dt, dev),
		gauge("system.io.wrqm_s", now, nonneg(c.writesMerged-p.writesMerged)/dt, dev),
		gauge("system.io.rkb_s", now, dRdSect/2/dt, dev), // sectors*512/1024 = sectors/2
		gauge("system.io.wkb_s", now, dWrSect/2/dt, dev),
		// Weighted ms over ms is iostat's avg_q_sz exactly (the stock check
		// divides by 1024, a historical quirk).
		gauge("system.io.avg_q_sz", now, nonneg(c.weightedMsIo-p.weightedMsIo)/(dt*1000), dev),
	}
	// %util: fraction of wall time the device had I/O in flight, capped at 100.
	util := (c.msIo - p.msIo) / (dt * 1000) * 100
	if util > 100 {
		util = 100
	}
	out = append(out, gauge("system.io.util", now, nonneg(util), dev))
	// Per-op latencies / sizes are undefined with no ops in the interval, so omit them.
	if ops := dReads + dWrites; ops > 0 {
		out = append(out,
			gauge("system.io.await", now, (dRdMs+dWrMs)/ops, dev),
			gauge("system.io.avg_rq_sz", now, (dRdSect+dWrSect)/ops, dev))
	}
	if dReads > 0 {
		out = append(out, gauge("system.io.r_await", now, dRdMs/dReads, dev))
	}
	if dWrites > 0 {
		out = append(out, gauge("system.io.w_await", now, dWrMs/dWrites, dev))
	}
	return out
}
