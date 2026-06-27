package host

import (
	"time"

	"golang.org/x/sys/unix"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// collectors returns the OpenBSD sub-collectors: the BSD-common cpu/load/uptime
// plus OpenBSD memory (vm.uvmexp) and disk usage (getfsstat). Per-device disk I/O
// (system.io.*) is not collected, it is FreeBSD-only among the BSDs. Verified live
// by e2e/vm_openbsd.sh.
func collectors(_ *Collector) []subCollector {
	return append(bsdCommon(), &openbsdMem{}, &openbsdDisk{}, &bsdIO{})
}

// memory: vm.uvmexp page counts, reported in MB like the stock Agent. OpenBSD's
// sysctl name table has no hw.physmem64, so total comes from Npages*Pagesize (the
// UVM-managed physical memory), which also matches the process agent's total.

type openbsdMem struct{}

func (openbsdMem) name() string { return "memory" }

func (openbsdMem) collect(now time.Time) ([]metrics.Serie, error) {
	u, err := unix.SysctlUvmexp("vm.uvmexp")
	if err != nil {
		return nil, err
	}
	pagesize := uint64(u.Pagesize)
	if pagesize == 0 {
		pagesize = 4096
	}
	total := uint64(u.Npages) * pagesize
	if total == 0 {
		return nil, nil
	}
	free := uint64(u.Free) * pagesize
	// Usable follows the stock Agent (gopsutil): free plus inactive, the pages
	// the kernel can hand out without swapping.
	usable := (uint64(u.Free) + uint64(u.Inactive)) * pagesize
	const mb = 1024.0 * 1024.0
	return []metrics.Serie{
		gauge("system.mem.total", now, float64(total)/mb),
		gauge("system.mem.free", now, float64(free)/mb),
		gauge("system.mem.used", now, float64(total-free)/mb),
		gauge("system.mem.usable", now, float64(usable)/mb),
		gauge("system.mem.pct_usable", now, float64(usable)/float64(total)),
	}, nil
}

// disk: getfsstat (struct statfs, F_-prefixed fields). Blocks count in F_bsize units.

type openbsdDisk struct{}

func (openbsdDisk) name() string { return "filesystem" }

func (openbsdDisk) collect(now time.Time) ([]metrics.Serie, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || n == 0 {
		return nil, err
	}
	buf := make([]unix.Statfs_t, n)
	if _, err := unix.Getfsstat(buf, unix.MNT_NOWAIT); err != nil {
		return nil, err
	}
	mounts := make([]bsdMount, 0, len(buf))
	for i := range buf {
		st := &buf[i]
		mounts = append(mounts, bsdMount{
			dev:    unix.ByteSliceToString(st.F_mntfromname[:]),
			fstype: unix.ByteSliceToString(st.F_fstypename[:]),
			blocks: st.F_blocks,
			bfree:  st.F_bfree,
			bavail: blocksAvail(st.F_bavail), // negative past the root reserve
			bsize:  uint64(st.F_bsize),
		})
	}
	return bsdDiskSeries(now, mounts), nil
}
