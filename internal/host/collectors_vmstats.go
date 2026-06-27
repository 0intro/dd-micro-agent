//go:build freebsd || dragonfly

package host

import (
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// vmstatsMem reports memory (MB) from sysctl hw.physmem plus the vm.stats.vm
// page counters, the layout FreeBSD and DragonFly share.
type vmstatsMem struct{}

func (vmstatsMem) name() string { return "memory" }

func (vmstatsMem) collect(now time.Time) ([]metrics.Serie, error) {
	total := sysctlNum("hw.physmem")
	if total == 0 {
		return nil, nil
	}
	pagesize := sysctlNum("hw.pagesize")
	if pagesize == 0 {
		pagesize = 4096
	}
	free := sysctlNum("vm.stats.vm.v_free_count") * pagesize
	// Usable follows the stock Agent (gopsutil): free plus inactive plus cache,
	// the memory reclaimable without swapping. v_cache_count is 0 on modern
	// kernels.
	usable := free +
		sysctlNum("vm.stats.vm.v_inactive_count")*pagesize +
		sysctlNum("vm.stats.vm.v_cache_count")*pagesize
	const mb = 1024.0 * 1024.0
	return []metrics.Serie{
		gauge("system.mem.total", now, float64(total)/mb),
		gauge("system.mem.free", now, float64(free)/mb),
		gauge("system.mem.used", now, float64(total-free)/mb),
		gauge("system.mem.usable", now, float64(usable)/mb),
		gauge("system.mem.pct_usable", now, float64(usable)/float64(total)),
	}, nil
}
