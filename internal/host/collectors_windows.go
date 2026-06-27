package host

import (
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

func collectors(_ *Collector) []subCollector {
	return []subCollector{&winCPU{}, winMem{}, winDisk{}, winUptime{}}
}

// Only the calls x/sys/windows has no typed wrapper for go through NewProc.
var (
	modkernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes       = modkernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
	procGetTickCount64       = modkernel32.NewProc("GetTickCount64")
)

func ft64(f windows.Filetime) uint64 {
	return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime)
}

// cpu: GetSystemTimes (kernel time includes idle), diffed to %

type winCPU struct {
	idle, kernel, user uint64
	has                bool
}

func (c *winCPU) name() string { return "cpu" }

func (c *winCPU) collect(now time.Time) ([]metrics.Serie, error) {
	var idle, kernel, user windows.Filetime
	r, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return nil, err
	}
	i, k, u := ft64(idle), ft64(kernel), ft64(user)

	out := []metrics.Serie{gauge("system.cpu.num_cores", now, float64(runtime.NumCPU()))}
	if c.has {
		di, dk, du := i-c.idle, k-c.kernel, u-c.user
		total := dk + du // kernel includes idle
		if total > 0 {
			pct := func(x uint64) float64 { return float64(x) / float64(total) * 100 }
			out = append(out,
				gauge("system.cpu.user", now, pct(du)),
				gauge("system.cpu.system", now, pct(dk-di)),
				gauge("system.cpu.idle", now, pct(di)),
			)
		}
	}
	c.idle, c.kernel, c.user = i, k, u
	c.has = true
	return out, nil
}

// memory: GlobalMemoryStatusEx, reported in MB

type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

type winMem struct{}

func (winMem) name() string { return "memory" }

func (winMem) collect(now time.Time) ([]metrics.Serie, error) {
	var m memoryStatusEx
	m.length = uint32(unsafe.Sizeof(m))
	if r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return nil, err
	}
	if m.totalPhys == 0 {
		return nil, nil
	}
	const mb = 1024.0 * 1024.0
	return []metrics.Serie{
		gauge("system.mem.total", now, float64(m.totalPhys)/mb),
		gauge("system.mem.free", now, float64(m.availPhys)/mb),
		gauge("system.mem.used", now, float64(m.totalPhys-m.availPhys)/mb),
		gauge("system.mem.pct_usable", now, float64(m.availPhys)/float64(m.totalPhys)),
	}, nil
}

// disk: GetLogicalDrives + GetDiskFreeSpaceExW, reported in kB

type winDisk struct{}

func (winDisk) name() string { return "filesystem" }

func (winDisk) collect(now time.Time) ([]metrics.Serie, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	var out []metrics.Serie
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		var avail, total, totalFree uint64
		if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &totalFree); err != nil || total == 0 {
			continue
		}
		const kb = 1024.0
		dev := root[:2] // e.g. "C:"
		tags := []string{"device:" + dev, "device_name:" + dev}
		used := float64(total-totalFree) / kb
		totalKB := float64(total) / kb
		out = append(out,
			gauge("system.disk.total", now, totalKB, tags...),
			gauge("system.disk.used", now, used, tags...),
			gauge("system.disk.free", now, float64(totalFree)/kb, tags...),
			// used/total, like the stock Agent on Windows (no root reserve).
			gauge("system.disk.in_use", now, ratio(used, totalKB), tags...),
		)
	}
	return out, nil
}

// uptime: GetTickCount64

type winUptime struct{}

func (winUptime) name() string { return "uptime" }

func (winUptime) collect(now time.Time) ([]metrics.Serie, error) {
	ms, _, _ := procGetTickCount64.Call()
	return []metrics.Serie{gauge("system.uptime", now, float64(uint64(ms))/1000)}, nil
}
