package host

import (
	"time"

	"golang.org/x/sys/unix"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// collectors returns the NetBSD sub-collectors: the BSD-common cpu/load/uptime
// plus NetBSD memory (vm.uvmexp2) and disk usage (getvfsstat). Per-device disk I/O
// (system.io.*) is not collected, it is FreeBSD-only among the BSDs. Verified live
// by e2e/vm_netbsd.sh.
func collectors(_ *Collector) []subCollector {
	return append(bsdCommon(), &netbsdMem{}, &netbsdDisk{}, &bsdIO{})
}

// memory: vm.uvmexp2 (struct uvmexp_sysctl, 64-bit fields), reported in MB. Total
// prefers hw.physmem64 (NetBSD resolves sysctl names at runtime) and falls back to
// Npages*Pagesize.

type netbsdMem struct{}

func (netbsdMem) name() string { return "memory" }

func (netbsdMem) collect(now time.Time) ([]metrics.Serie, error) {
	u, err := unix.SysctlUvmexp("vm.uvmexp2")
	if err != nil {
		return nil, err
	}
	pagesize := uint64(u.Pagesize)
	if pagesize == 0 {
		pagesize = 4096
	}
	total := sysctlNum("hw.physmem64")
	if total == 0 {
		total = uint64(u.Npages) * pagesize
	}
	if total == 0 {
		return nil, nil
	}
	free := uint64(u.Free) * pagesize
	// Usable follows the stock Agent (gopsutil): free plus inactive plus the
	// file/exec page cache, the memory reclaimable without swapping.
	usable := (uint64(u.Free) + uint64(u.Inactive) + uint64(u.Filepages) + uint64(u.Execpages)) * pagesize
	const mb = 1024.0 * 1024.0
	return []metrics.Serie{
		gauge("system.mem.total", now, float64(total)/mb),
		gauge("system.mem.free", now, float64(free)/mb),
		gauge("system.mem.used", now, float64(total-free)/mb),
		gauge("system.mem.usable", now, float64(usable)/mb),
		gauge("system.mem.pct_usable", now, float64(usable)/float64(total)),
	}, nil
}

// disk: getvfsstat (struct statvfs). statvfs counts blocks in Frsize (fragment) units.

type netbsdDisk struct{}

func (netbsdDisk) name() string { return "filesystem" }

func (netbsdDisk) collect(now time.Time) ([]metrics.Serie, error) {
	n, err := unix.Getvfsstat(nil, unix.ST_NOWAIT)
	if err != nil || n == 0 {
		return nil, err
	}
	buf := make([]unix.Statvfs_t, n)
	if _, err := unix.Getvfsstat(buf, unix.ST_NOWAIT); err != nil {
		return nil, err
	}
	mounts := make([]bsdMount, 0, len(buf))
	for i := range buf {
		st := &buf[i]
		unit := st.Frsize
		if unit == 0 {
			unit = st.Bsize
		}
		mounts = append(mounts, bsdMount{
			dev:    unix.ByteSliceToString(st.Mntfromname[:]),
			fstype: unix.ByteSliceToString(st.Fstypename[:]),
			blocks: st.Blocks,
			bfree:  st.Bfree,
			bavail: st.Bavail,
			bsize:  unit,
		})
	}
	return bsdDiskSeries(now, mounts), nil
}
