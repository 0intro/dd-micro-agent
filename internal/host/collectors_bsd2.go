//go:build openbsd || netbsd

package host

import (
	"path/filepath"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// Disk usage on OpenBSD and NetBSD. The two kernels list mounted filesystems
// through different calls and structs (OpenBSD getfsstat + struct statfs, NetBSD
// getvfsstat + struct statvfs), so each collectors_<os>.go fills this neutral
// bsdMount from its own struct and hands the slice to bsdDiskSeries, which owns
// the dedup, the synthetic-filesystem skip, and the series shapes. Per-device disk
// I/O (system.io.*) stays FreeBSD-only among the BSDs and is not collected here.

// bsdMount is one filesystem's usage in block units, OS independent.
type bsdMount struct {
	dev    string // mount source (device path), lifted into device:/device_name:
	fstype string // filesystem type name, used to drop synthetic mounts
	blocks uint64 // total blocks
	bfree  uint64 // blocks free (includes the root reserve)
	bavail uint64 // blocks available to non-root
	bsize  uint64 // block size in bytes
}

// pseudoBSD2FS are synthetic filesystems with no real capacity. Most also report
// zero blocks (already skipped), this catches the rest.
var pseudoBSD2FS = map[string]bool{
	"kernfs": true, "procfs": true, "ptyfs": true, "tmpfs": true,
	"mfs": true, "fdesc": true, "null": true, "overlay": true, "fuse": true,
}

// bsdDiskSeries turns the mounts into system.disk.* gauges (kB), one device once.
// It mirrors unixDisk (darwin/freebsd): total/used/free plus in_use as the stock
// Agent's UsedPercent, tagged device: and device_name:.
func bsdDiskSeries(now time.Time, mounts []bsdMount) []metrics.Serie {
	var out []metrics.Serie
	seen := map[string]bool{}
	const kb = 1024.0
	for _, m := range mounts {
		if m.blocks == 0 || seen[m.dev] || pseudoBSD2FS[m.fstype] {
			continue
		}
		seen[m.dev] = true
		total := float64(m.blocks*m.bsize) / kb
		free := float64(m.bavail*m.bsize) / kb
		used := float64((m.blocks-m.bfree)*m.bsize) / kb
		tags := []string{"device:" + m.dev, "device_name:" + filepath.Base(m.dev)}
		out = append(out,
			gauge("system.disk.total", now, total, tags...),
			gauge("system.disk.used", now, used, tags...),
			gauge("system.disk.free", now, free, tags...),
			// used/(used+free) excludes the root reserve, like the stock Agent.
			gauge("system.disk.in_use", now, ratio(used, used+free), tags...),
		)
	}
	return out
}
