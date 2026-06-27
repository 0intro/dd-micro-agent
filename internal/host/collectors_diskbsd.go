//go:build darwin || freebsd || dragonfly

package host

import (
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// unixDisk reports per-filesystem usage (kB) via getfsstat. darwin and freebsd
// share Statfs_t field names, uint64 casts paper over their signedness/width
// differences (freebsd Bavail is int64, darwin Bsize is uint32).
type unixDisk struct{}

func (unixDisk) name() string { return "filesystem" }

var pseudoBSDFS = map[string]bool{
	"devfs": true, "procfs": true, "fdescfs": true,
	"linprocfs": true, "linsysfs": true, "tmpfs": true, "nullfs": true,
}

func (unixDisk) collect(now time.Time) ([]metrics.Serie, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || n == 0 {
		return nil, err
	}
	buf := make([]unix.Statfs_t, n)
	if _, err := unix.Getfsstat(buf, unix.MNT_NOWAIT); err != nil {
		return nil, err
	}
	var out []metrics.Serie
	seen := make(map[string]bool)
	for i := range buf {
		st := &buf[i]
		dev := unix.ByteSliceToString(st.Mntfromname[:])
		if st.Blocks == 0 || seen[dev] || pseudoBSDFS[unix.ByteSliceToString(st.Fstypename[:])] {
			continue
		}
		seen[dev] = true
		bsize := uint64(st.Bsize)
		const kb = 1024.0
		total := float64(uint64(st.Blocks)*bsize) / kb
		free := float64(blocksAvail(int64(st.Bavail))*bsize) / kb
		used := float64((uint64(st.Blocks)-uint64(st.Bfree))*bsize) / kb
		tags := []string{"device:" + dev, "device_name:" + filepath.Base(dev)}
		out = append(out,
			gauge("system.disk.total", now, total, tags...),
			gauge("system.disk.used", now, used, tags...),
			gauge("system.disk.free", now, free, tags...),
			// used/(used+free) is the stock Agent's UsedPercent: the root
			// reserve is excluded, so a filesystem full for non-root reads 1.0.
			gauge("system.disk.in_use", now, ratio(used, used+free), tags...),
		)
	}
	return out, nil
}
